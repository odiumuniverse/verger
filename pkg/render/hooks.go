package render

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/odiumuniverse/verger/pkg/manifest"
)

// HookPlan is the merge of canonical hooks into one host hooks object:
// foreign entries are preserved, verger's entries are replaced.
type HookPlan struct {
	File     []byte // new hooks object (nil when unchanged)
	Rendered int
	Owned    []string // commands rendered by this plan (RMA/ownership)
	Warnings []string
}

// PlanHooks renders canonical hooks into the hooks object of a host dialect.
// The value for the host's `hooks` key is returned as strict JSON, ready for
// EditJSONC path "hooks"; foreign entries inside the hooks object are kept.
// existing may be the whole config document or the bare hooks object, and nil
// when the file does not exist.
func PlanHooks(format manifest.Format, existing []byte, hooks []manifest.Hook) (HookPlan, error) {
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

		entry, dropped := hookEntry(hook, manifest.MatcherEvent(format, hook.Event))
		if dropped {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s hooks: matcher %q is not expressible on %s; dropped", format, hook.Matcher, event))
		}

		rendered[event] = append(rendered[event], entry)
		plan.Rendered++

		if !ownedSeen[hook.Command] {
			ownedSeen[hook.Command] = true

			plan.Owned = append(plan.Owned, hook.Command)
		}
	}

	merged, warns := mergeHooksObject(existingHooks, rendered, format)

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
func mergeHooksObject(existing map[string]any, rendered map[string][]any, format manifest.Format) (map[string]any, []string) {
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
		value, eventWarns := mergeHookEvent(event, existing[event], rendered[event], renderedCommands, format)

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
func mergeHookEvent(event string, existing any, fresh []any, renderedCommands map[string]bool, format manifest.Format) (any, []string) {
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

	return mergeHookEntries(event, list, fresh, renderedCommands, format)
}

// mergeHookEntries rebuilds one event's array.
func mergeHookEntries(event string, list, fresh []any, renderedCommands map[string]bool, format manifest.Format) (any, []string) {
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

// hookEntry renders one canonical hook as a nested matcher group.
func hookEntry(hook manifest.Hook, matcherEvent bool) (any, bool) {
	matcher := hook.Matcher
	if matcher == "*" {
		matcher = ""
	}

	dropped := matcher != "" && !matcherEvent
	if dropped {
		matcher = ""
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

// hookEntryCommands lists the commands one hooks entry carries.
func hookEntryCommands(entry any) ([]string, bool) {
	object, ok := entry.(map[string]any)
	if !ok {
		return nil, false
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
