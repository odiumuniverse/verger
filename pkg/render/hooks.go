package render

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/manifest"
)

// HookPlan is the merge of canonical hooks into one host hooks object:
// foreign entries are preserved, verger's entries are replaced.
type HookPlan struct {
	File     []byte // new hooks object (nil when unchanged)
	Rendered int
	Owned    []string // commands rendered by this plan (RMA/ownership)
	// Events lists the records this plan wrote, event by event, so the
	// caller can claim one receipt artifact per record instead of one for
	// the whole document.
	Events   []HookEventPlan
	Warnings []string
	// Refused names the records this plan left alone because their bytes on
	// disk are not the ones the receipt recorded. It is structured, not
	// folded into Warnings, because a refused record is a verdict the caller
	// has to report as hands-off — a warning in a note list reads as success
	// to everything downstream, including a script checking the exit code.
	Refused []string
}

// HookEventPlan is one event's records as this plan wrote them.
type HookEventPlan struct {
	// Name is the host's own event name.
	Name string

	// Records are the entries appended for it.
	Records []HookRecordPlan
}

// HookRecordPlan is one record as this plan wrote it.
type HookRecordPlan struct {
	// Name is the command, which is the record's identity.
	Name string

	// Digest is the canonical JSON of the record as it appears in the
	// document: what a receipt stores so a later delivery can tell a hand
	// edit from its own bytes.
	Digest digest.Hash
}

// PlanHooks renders canonical hooks into the hooks object of a host dialect.
// The value for the host's `hooks` key is returned as strict JSON, ready for
// EditJSONC path "hooks"; foreign entries inside the hooks object are kept.
// existing may be the whole config document or the bare hooks object, and nil
// when the file does not exist. `existing` must be strict JSON: a JSONC caller
// (a settings.json with comments or trailing commas) standardizes the document
// first (hujson) and passes the `hooks` member.
func PlanHooks(format manifest.Format, existing []byte, hooks []manifest.Hook, owned Owned) (HookPlan, error) {
	events, ok := hookEventsFor(format)
	if !ok {
		return HookPlan{}, &RenderError{Kind: string(format), Name: "hooks", Cause: errors.New("the format has no hook dialect")}
	}

	existingHooks, err := existingHooksObject(existing)
	if err != nil {
		return HookPlan{}, err
	}

	var (
		plan      HookPlan
		rendered  = map[string][]any{}
		ownedSeen = map[string]bool{}
	)

	for _, hook := range sortHooks(hooks) {
		event, ok := events[hook.Event]
		if !ok {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s hooks: hook event %s has no %s event; skipped", format, hook.Event, format))

			continue
		}

		entry, dropped := hookEntry(hook, format, manifest.MatcherEvent(format, hook.Event))
		if dropped {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s hooks: matcher %q is not expressible on %s; dropped", format, hook.Matcher, event))
		}

		rendered[event] = append(rendered[event], entry)
		plan.Rendered++

		plan.Events = append(plan.Events, HookEventPlan{Name: event})
		last := len(plan.Events) - 1

		plan.Events[last].Records = append(plan.Events[last].Records, HookRecordPlan{
			Name:   hook.Command,
			Digest: HookRecordDigest(entry),
		})

		if !ownedSeen[hook.Command] {
			ownedSeen[hook.Command] = true

			plan.Owned = append(plan.Owned, hook.Command)
		}
	}

	merged, warns := mergeHooksObject(existingHooks, rendered, format, owned, &plan.Refused)

	plan.Warnings = append(plan.Warnings, warns...)

	if len(merged) == 0 || sameHooksObject(merged, existingHooks) {
		return plan, nil
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return HookPlan{}, fmt.Errorf("encode hooks: %w", err)
	}

	data = append(data, '\n')
	plan.File = data

	return plan, nil
}

// hookEventsFor returns the canonical-to-host event table of a dialect.
func hookEventsFor(format manifest.Format) (map[string]string, bool) {
	switch format {
	case manifest.FormatClaude, manifest.FormatCodex:
		return map[string]string{
			manifest.EventPreTool:      "PreToolUse",
			manifest.EventPostTool:     "PostToolUse",
			manifest.EventSessionStart: "SessionStart",
			manifest.EventStop:         "Stop",
			manifest.EventNotification: "Notification",
		}, true
	case manifest.FormatGemini:
		return map[string]string{
			manifest.EventPreTool:      "BeforeTool",
			manifest.EventPostTool:     "AfterTool",
			manifest.EventSessionStart: "SessionStart",
			manifest.EventNotification: "Notification",
		}, true
	case manifest.FormatCursor:
		// The event names are quoted from the live cursor-agent bundle
		// 2026.06.15-18-00-12-6f5a2cf (chunk 2097.index.js, its own event
		// enum). Every canonical event has an equivalent, so nothing is
		// silenced for a missing one; `beforeSubmitPrompt` is the closest
		// Cursor has to a notification and is a chosen mapping, not a
		// semantic identity.
		return map[string]string{
			manifest.EventPreTool:      "preToolUse",
			manifest.EventPostTool:     "postToolUse",
			manifest.EventSessionStart: "sessionStart",
			manifest.EventStop:         "stop",
			manifest.EventNotification: "beforeSubmitPrompt",
		}, true
	default:
		return nil, false
	}
}

// existingHooksObject extracts the hooks object of a config document: the
// top-level `hooks` member, the document itself when every value is an array,
// or nil.
func existingHooksObject(existing []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return nil, nil
	}

	doc := map[string]any{}

	if err := json.Unmarshal(existing, &doc); err != nil {
		return nil, &ConfigParseError{Cause: err}
	}

	if raw, ok := doc["hooks"]; ok {
		if raw == nil {
			return nil, nil
		}

		hooks, ok := raw.(map[string]any)
		if !ok {
			return nil, &ConfigParseError{Cause: errors.New("the hooks member is not an object")}
		}

		return hooks, nil
	}

	for _, value := range doc {
		if _, ok := value.([]any); !ok {
			return nil, nil
		}
	}

	return doc, nil
}

// mergeHooksObject rebuilds the hooks object: our entries are replaced by the
// fresh render, foreign entries and foreign handlers stay.
func mergeHooksObject(existing map[string]any, rendered map[string][]any, format manifest.Format, owned Owned, refused *[]string) (map[string]any, []string) {
	renderedCommands := hookCommandSet(rendered)

	events := map[string]bool{}

	for event := range existing {
		events[event] = true
	}

	for event := range rendered {
		events[event] = true
	}

	var warns []string

	out := map[string]any{}

	for _, event := range sortedKeys(events) {
		value, eventWarns := mergeHookEvent(event, existing[event], rendered[event], renderedCommands, format, owned, refused)

		warns = append(warns, eventWarns...)

		if value != nil {
			out[event] = value
		}
	}

	return out, warns
}

// hookCommandSet collects every command the fresh render writes.
func hookCommandSet(rendered map[string][]any) map[string]bool {
	out := map[string]bool{}

	for _, entries := range rendered {
		for _, entry := range entries {
			commands, ok := hookEntryCommands(entry)
			if !ok {
				continue
			}

			for _, command := range commands {
				out[command] = true
			}
		}
	}

	return out
}

// mergeHookEvent merges one event: a malformed value stays untouched, entries
// ours are replaced, mixed and foreign entries are preserved.
func mergeHookEvent(event string, existing any, fresh []any, renderedCommands map[string]bool, format manifest.Format, owned Owned, refused *[]string) (any, []string) {
	if existing != nil {
		if _, isList := existing.([]any); !isList {
			if len(fresh) == 0 {
				return existing, nil
			}

			return existing, []string{fmt.Sprintf(
				"%s hooks: event %s is not an array; left untouched, %d hook(s) not rendered", format, event, len(fresh))}
		}
	}

	list, _ := existing.([]any)

	return mergeHookEntries(event, list, fresh, renderedCommands, format, owned, refused)
}

// mergeHookEntries rebuilds one event's array.
func mergeHookEntries(event string, list, fresh []any, renderedCommands map[string]bool, format manifest.Format, owned Owned, refused *[]string) (any, []string) {
	var (
		next  []any
		warns []string
	)

	for _, entry := range list {
		commands, ok := hookEntryCommands(entry)
		if !ok {
			next = append(next, entry)

			continue
		}

		ours := 0

		for _, command := range commands {
			if renderedCommands[command] {
				ours++
			}
		}

		switch {
		case ours == len(commands) && handEditedHookEntry(event, entry, commands, owned):
			// Ours by command, but the bytes on disk are not the bytes the
			// receipt recorded: the user edited this record, so it stays.
			next = append(next, entry)
			*refused = append(*refused, hookRecordLabel(commands))
			warns = append(warns, fmt.Sprintf(
				"%s hooks: %s: the record changed outside verger; left in place", format, hookRecordLabel(commands)))
		case ours == len(commands):
			// Ours: the fresh render replaces it.
		case ours > 0:
			next = append(next, foreignHookHandlers(entry, renderedCommands))
			warns = append(warns, fmt.Sprintf(
				"%s hooks: a hook group on %s mixes verger and foreign handlers; the foreign handlers stay and verger's handlers are re-rendered", format, event))
		default:
			next = append(next, entry)
		}
	}

	next = append(next, fresh...)

	if len(next) == 0 {
		return nil, warns
	}

	return next, warns
}

// hookEntry renders one canonical hook in the host's own dialect. Cursor is
// flat: a `{command, timeout?, matcher?}` record with no `type` and no nested
// handler group, read from the live cursor-agent bundle
// 2026.06.15-18-00-12-6f5a2cf (chunk 2097.index.js: the loader reads
// `command`, a numeric `timeout` and a `matcher`; it also reads `failClosed`,
// which a foreign record keeps and we never write because the canonical hook
// has no field for it).
func hookEntry(hook manifest.Hook, format manifest.Format, matcherEvent bool) (any, bool) {
	matcher := hook.Matcher
	if matcher == "*" {
		matcher = ""
	}

	dropped := matcher != "" && !matcherEvent
	if dropped {
		matcher = ""
	}

	if format == manifest.FormatCursor {
		record := map[string]any{keyCommand: hook.Command}
		if hook.Timeout > 0 {
			record["timeout"] = hook.Timeout
		}

		if matcher != "" {
			record["matcher"] = matcher
		}

		return record, dropped
	}

	handler := map[string]any{keyType: kindCommand, keyCommand: hook.Command}
	if hook.Timeout > 0 {
		handler["timeout"] = hook.Timeout
	}

	group := map[string]any{"hooks": []any{handler}}
	if matcher != "" {
		group["matcher"] = matcher
	}

	return group, dropped
}

// hookEntryCommands lists the commands one hooks entry carries. It reads both
// shapes: the nested `{hooks:[…]}` group of the Claude/Codex/Gemini dialects,
// and Cursor's flat record, which carries the command itself.
func hookEntryCommands(entry any) ([]string, bool) {
	object, ok := entry.(map[string]any)
	if !ok {
		return nil, false
	}

	if command, ok := object[keyCommand].(string); ok {
		return []string{command}, true
	}

	handlers, ok := object["hooks"].([]any)
	if !ok || len(handlers) == 0 {
		return nil, false
	}

	commands := make([]string, 0, len(handlers))

	for _, handler := range handlers {
		object, ok := handler.(map[string]any)
		if !ok {
			return nil, false
		}

		command, ok := object[keyCommand].(string)
		if !ok {
			return nil, false
		}

		commands = append(commands, command)
	}

	return commands, true
}

// foreignHookHandlers returns the entry with the handlers verger owns removed.
func foreignHookHandlers(entry any, owned map[string]bool) any {
	object, _ := entry.(map[string]any)
	handlers, _ := object["hooks"].([]any)

	kept := make([]any, 0, len(handlers))

	for _, handler := range handlers {
		if inner, ok := handler.(map[string]any); ok {
			if command, ok := inner[keyCommand].(string); ok && owned[command] {
				continue
			}
		}

		kept = append(kept, handler)
	}

	clone := maps.Clone(object)
	clone["hooks"] = kept

	return clone
}

// sameHooksObject reports whether two hooks objects carry the same data.
func sameHooksObject(left, right map[string]any) bool {
	leftData, leftErr := json.Marshal(left)
	if leftErr != nil {
		return false
	}

	rightData, rightErr := json.Marshal(right)
	if rightErr != nil {
		return false
	}

	return bytes.Equal(leftData, rightData)
}

// sortHooks copies and orders hooks by (Event, Matcher, Command, Timeout).
func sortHooks(hooks []manifest.Hook) []manifest.Hook {
	out := slices.Clone(hooks)

	slices.SortFunc(out, func(a, b manifest.Hook) int {
		return cmp.Or(
			cmp.Compare(a.Event, b.Event),
			cmp.Compare(a.Matcher, b.Matcher),
			cmp.Compare(a.Command, b.Command),
			cmp.Compare(a.Timeout, b.Timeout),
		)
	})

	return out
}

// hookRecordKey is the ownership key of one hook record: the host event and the
// command that identifies it, which is what the receipt records a digest for.
// A record's `matcher` and `timeout` are fields *of* that record, not part of
// its identity — an edit to either is exactly the hand edit the digest catches.
func hookRecordKey(event, command string) string {
	return "hooks/" + event + "/" + command
}

// handEditedHookEntry reports whether a record verger owns by command was
// changed outside verger: the receipt holds a digest for it and the record on
// disk no longer hashes to it. With no recorded digest the record is treated as
// the user's, never as ours, so a first delivery over a pre-existing document
// cannot claim a record verger did not write.
func handEditedHookEntry(event string, entry any, commands []string, owned Owned) bool {
	if len(owned) == 0 || len(commands) != 1 {
		return false
	}

	recorded, ok := owned[hookRecordKey(event, commands[0])]
	if !ok || recorded == "" {
		return false
	}

	return recorded != HookRecordDigest(entry)
}

// hookRecordLabel names a record in a note: the command, which is its identity.
func hookRecordLabel(commands []string) string {
	if len(commands) == 0 {
		return "a hook record"
	}

	return commands[0]
}

// HookRecordDigest is the digest a receipt stores for one rendered hook record,
// so a later delivery can tell "the user edited this" from "this is what I
// wrote". It is the canonical JSON of the record as it appears in the document.
func HookRecordDigest(entry any) digest.Hash {
	object, ok := entry.(map[string]any)
	if !ok {
		return ""
	}

	data, err := json.Marshal(object)
	if err != nil {
		return ""
	}

	return digest.Bytes(data)
}

// HookEventName returns the host event name a canonical event renders to in a
// dialect, and whether the dialect has one at all. It is the single table the
// renderer and a delivery planner share, so a receipt key can never be built
// from a different spelling than the document uses.
func HookEventName(format manifest.Format, canonEvent string) (string, bool) {
	events, ok := hookEventsFor(format)
	if !ok {
		return "", false
	}

	name, ok := events[canonEvent]

	return name, ok
}
