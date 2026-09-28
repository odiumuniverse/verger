package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Canonical hook events.
const (
	EventNotification = "notification"
	EventPostTool     = "post-tool"
	EventPreTool      = "pre-tool"
	EventSessionStart = "session-start"
	EventStop         = "stop"
)

// canonEventsFor returns the dialect table of one format: host event names to
// canonical events. An unmapped host event is never guessed.
func canonEventsFor(format Format) map[string]string {
	switch format {
	case FormatClaude, FormatCodex:
		return map[string]string{
			"PreToolUse":   EventPreTool,
			"PostToolUse":  EventPostTool,
			"SessionStart": EventSessionStart,
			"Stop":         EventStop,
			"Notification": EventNotification,
		}
	case FormatGemini:
		return map[string]string{
			"BeforeTool":   EventPreTool,
			"AfterTool":    EventPostTool,
			"SessionStart": EventSessionStart,
			"Notification": EventNotification,
		}
	default:
		return nil
	}
}

// ValidEvent reports whether event is a canonical hook event.
func ValidEvent(event string) bool {
	switch event {
	case EventNotification, EventPostTool, EventPreTool, EventSessionStart, EventStop:
		return true
	default:
		return false
	}
}

// CanonEvent maps a host event name of one dialect to the canonical event.
func CanonEvent(format Format, hostEvent string) (string, bool) {
	canon, ok := canonEventsFor(format)[hostEvent]

	return canon, ok
}

// HostEvent maps a canonical event to the host event name of one dialect.
func HostEvent(format Format, canonEvent string) (string, bool) {
	for hostEvent, canon := range canonEventsFor(format) {
		if canon == canonEvent {
			return hostEvent, true
		}
	}

	return "", false
}

// MatcherEvent reports whether a canonical event carries a matcher in the
// dialect: only pre-tool and post-tool do.
func MatcherEvent(format Format, canonEvent string) bool {
	switch format {
	case FormatClaude, FormatCodex, FormatGemini, FormatCursor:
		return canonEvent == EventPreTool || canonEvent == EventPostTool
	default:
		return false
	}
}

// hookHandler is one raw handler before the canon filters it.
type hookHandler struct {
	Type        string
	Command     string
	Timeout     int
	Unsupported []string
}

// handlerKeys are the handler fields the reader understands; shell and
// statusMessage are cosmetic and args is understood only when empty.
func isHandlerKey(key string) bool {
	switch key {
	case "type", "command", "timeout", "shell", "statusMessage", "args":
		return true
	default:
		return false
	}
}

// hooksFromDocument parses a hooks document in the dialect of format: the
// {"hooks": {...}} wrapper or a bare event map. Hooks are returned sorted and
// every skipped definition is reported as a warning.
func hooksFromDocument(format Format, file string, data []byte) ([]Hook, []string, error) {
	var raw map[string]json.RawMessage

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse hooks: %w", err)
	}

	events := raw

	if wrapper, ok := raw["hooks"]; ok {
		events = map[string]json.RawMessage{}

		if err := json.Unmarshal(wrapper, &events); err != nil {
			return nil, nil, fmt.Errorf("parse hooks: %w", err)
		}
	}

	var (
		hooks []Hook
		warns []string
	)

	for _, event := range slices.Sorted(maps.Keys(events)) {
		if isMetaHookKey(event) {
			continue
		}

		canon, ok := CanonEvent(format, event)
		if !ok {
			warns = append(warns, warning(format, file, fmt.Sprintf("hook event %s has no canonical mapping; skipped", event)))

			continue
		}

		var entries []json.RawMessage

		if err := json.Unmarshal(events[event], &entries); err != nil {
			warns = append(warns, warning(format, file, fmt.Sprintf("hook event %s is not a list of matcher groups; ignored", event)))

			continue
		}

		for _, entry := range entries {
			entryHooks, entryWarns := hooksFromEntry(format, file, canon, entry)

			warns = append(warns, entryWarns...)
			hooks = append(hooks, entryHooks...)
		}
	}

	return sortHooks(hooks), warns, nil
}

// metaHookKeys are document keys that carry metadata rather than an event.
func isMetaHookKey(key string) bool {
	switch key {
	case "$schema", "description", "version":
		return true
	default:
		return false
	}
}

// hooksFromEntry decodes one entry of an event list: a nested matcher group or
// a flat handler normalized into a single-handler group.
func hooksFromEntry(format Format, file, canon string, data json.RawMessage) ([]Hook, []string) {
	var raw map[string]json.RawMessage

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, []string{warning(format, file, "hook entry is not an object; ignored")}
	}

	if _, grouped := raw["hooks"]; grouped {
		return hooksFromGroup(format, file, canon, raw)
	}

	if _, flat := raw["command"]; !flat {
		return nil, []string{warning(format, file, "hook entry is neither a matcher group nor a handler; ignored")}
	}

	matcher, matcherWarns := handlerMatcher(format, file, raw)

	handlerData, err := json.Marshal(withDefaultType(raw))
	if err != nil {
		return nil, append(matcherWarns, warning(format, file, "hook handler cannot be decoded; ignored"))
	}

	handler, handlerWarns, ok := decodeHandler(format, file, canon, matcher, handlerData)
	if !ok {
		return nil, append(matcherWarns, handlerWarns...)
	}

	return []Hook{handler}, matcherWarns
}

// hooksFromGroup decodes one nested matcher group.
func hooksFromGroup(format Format, file, canon string, raw map[string]json.RawMessage) ([]Hook, []string) {
	matcher, warns := handlerMatcher(format, file, raw)

	var handlers []json.RawMessage

	if err := json.Unmarshal(raw["hooks"], &handlers); err != nil {
		return nil, append(warns, warning(format, file, "hook entry is not a matcher group; ignored"))
	}

	var hooks []Hook

	for _, handler := range handlers {
		hook, handlerWarns, ok := decodeHandler(format, file, canon, matcher, handler)
		if !ok {
			warns = append(warns, handlerWarns...)

			continue
		}

		hooks = append(hooks, hook)
	}

	return hooks, warns
}

// handlerMatcher reads the matcher of a group or flat handler; "*" and empty
// are both kept as "".
func handlerMatcher(format Format, file string, raw map[string]json.RawMessage) (string, []string) {
	value, ok := raw["matcher"]
	if !ok {
		return "", nil
	}

	var matcher string

	if err := json.Unmarshal(value, &matcher); err != nil {
		return "", []string{warning(format, file, "hook matcher is not a string; ignored")}
	}

	if matcher == "*" {
		return "", nil
	}

	return matcher, nil
}

// withDefaultType gives a flat handler the documented default type.
func withDefaultType(raw map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(raw)+1)

	maps.Copy(out, raw)

	delete(out, "matcher")

	if _, ok := out["type"]; !ok {
		out["type"] = json.RawMessage(`"command"`)
	}

	return out
}

// decodeHandler filters one handler: the canon expresses a single shell
// command with a timeout, everything else is skipped with a warning.
func decodeHandler(format Format, file, canon, matcher string, data json.RawMessage) (Hook, []string, bool) {
	handler, err := decodeHookHandler(data)
	if err != nil {
		return Hook{}, []string{warning(format, file, "hook handler cannot be decoded; ignored")}, false
	}

	if handler.Type != "command" {
		return Hook{}, []string{
			warning(format, file, fmt.Sprintf("hook %s handler type %q is not a command; skipped", canon, handler.Type)),
		}, false
	}

	if len(handler.Unsupported) > 0 {
		return Hook{}, []string{
			warning(format, file, fmt.Sprintf("hook %s handler has unsupported field(s) %s; skipped", canon, strings.Join(handler.Unsupported, ", "))),
		}, false
	}

	if strings.TrimSpace(handler.Command) == "" {
		return Hook{}, []string{warning(format, file, fmt.Sprintf("hook %s handler has no command; skipped", canon))}, false
	}

	return Hook{Event: canon, Matcher: matcher, Command: handler.Command, Timeout: handler.Timeout, Origin: format}, nil, true
}

// decodeHookHandler decodes one handler, recording every field the canon
// cannot express.
func decodeHookHandler(data json.RawMessage) (hookHandler, error) {
	var raw map[string]json.RawMessage

	if err := json.Unmarshal(data, &raw); err != nil {
		return hookHandler{}, err
	}

	handler := hookHandler{}

	for _, key := range slices.Sorted(maps.Keys(raw)) {
		if !isHandlerKey(key) {
			handler.Unsupported = append(handler.Unsupported, key)
		}
	}

	decodeType(&handler, raw)
	decodeCommand(&handler, raw)
	decodeTimeout(&handler, raw)
	decodeArgs(&handler, raw)

	return handler, nil
}

// decodeType reads the handler type.
func decodeType(handler *hookHandler, raw map[string]json.RawMessage) {
	value, ok := raw["type"]
	if !ok || json.Unmarshal(value, &handler.Type) != nil {
		handler.Unsupported = append(handler.Unsupported, "type")
	}
}

// decodeCommand reads the handler command; a missing command is reported by
// the explicit empty-command check, a malformed one as an unsupported field.
func decodeCommand(handler *hookHandler, raw map[string]json.RawMessage) {
	value, ok := raw["command"]
	if !ok {
		return
	}

	if json.Unmarshal(value, &handler.Command) != nil {
		handler.Unsupported = append(handler.Unsupported, "command")
	}
}

// decodeTimeout reads the timeout; values at or below zero become zero, an
// unrepresentable value is an unsupported field.
func decodeTimeout(handler *hookHandler, raw map[string]json.RawMessage) {
	value, ok := raw["timeout"]
	if !ok {
		return
	}

	var timeout float64

	if err := json.Unmarshal(value, &timeout); err != nil || timeout > math.MaxInt32 || math.IsNaN(timeout) {
		handler.Unsupported = append(handler.Unsupported, "timeout")

		return
	}

	if timeout > 0 {
		handler.Timeout = int(timeout)
	}
}

// decodeArgs accepts an absent or empty args list only.
func decodeArgs(handler *hookHandler, raw map[string]json.RawMessage) {
	value, ok := raw["args"]
	if !ok {
		return
	}

	var args []string

	if json.Unmarshal(value, &args) != nil || len(args) > 0 {
		handler.Unsupported = append(handler.Unsupported, "args")
	}
}

// hooksFromPath reads an optional hooks document of one dialect.
func hooksFromPath(format Format, root, rel string, warnings *[]string) []Hook {
	data, state, err := readEntry(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		*warnings = append(*warnings, warning(format, rel, "cannot read: "+err.Error()))

		return nil
	}

	switch state {
	case entryFile:
	case entryMissing:
		return nil
	case entrySymlink:
		*warnings = append(*warnings, warning(format, rel, "symlink is never followed; ignored"))

		return nil
	default:
		*warnings = append(*warnings, warning(format, rel, "not a regular file; ignored"))

		return nil
	}

	hooks, warns, err := hooksFromDocument(format, rel, data)
	if err != nil {
		*warnings = append(*warnings, warning(format, rel, err.Error()))

		return nil
	}

	*warnings = append(*warnings, warns...)

	return hooks
}

// hooksFromInline reads the `hooks` field of a manifest: an inline event map
// or a plugin-relative path to a document. The field is optional.
func hooksFromInline(format Format, root, file string, raw json.RawMessage, warnings *[]string) []Hook {
	if len(raw) == 0 {
		return nil
	}

	var inline map[string]json.RawMessage

	if json.Unmarshal(raw, &inline) == nil {
		hooks, warns, err := hooksFromDocument(format, file, raw)
		if err != nil {
			*warnings = append(*warnings, warning(format, file, err.Error()))

			return nil
		}

		*warnings = append(*warnings, warns...)

		return hooks
	}

	var rel string

	if json.Unmarshal(raw, &rel) == nil {
		return hooksFromPathString(format, root, file, rel, warnings)
	}

	*warnings = append(*warnings, warning(format, file, "the hooks field is neither an inline map nor a path; ignored"))

	return nil
}

// hookSources reads an explicit hooks declaration: a path (or a list of
// paths), an inline document (or a list), or a mix.
func hookSources(format Format, root, file string, raw json.RawMessage, warnings *[]string) []Hook {
	if len(raw) == 0 {
		return nil
	}

	var asString string

	if json.Unmarshal(raw, &asString) == nil {
		return hooksFromPathString(format, root, file, asString, warnings)
	}

	var asList []json.RawMessage

	if json.Unmarshal(raw, &asList) == nil {
		var hooks []Hook

		for _, item := range asList {
			hooks = append(hooks, hookSourceItem(format, root, file, item, warnings)...)
		}

		return sortHooks(hooks)
	}

	hooks, warns, err := hooksFromDocument(format, file, raw)
	if err != nil {
		*warnings = append(*warnings, warning(format, file, err.Error()))

		return nil
	}

	*warnings = append(*warnings, warns...)

	return hooks
}

// hookSourceItem reads one item of a hooks list: a path or an inline document.
func hookSourceItem(format Format, root, file string, item json.RawMessage, warnings *[]string) []Hook {
	var rel string

	if json.Unmarshal(item, &rel) == nil {
		return hooksFromPathString(format, root, file, rel, warnings)
	}

	hooks, warns, err := hooksFromDocument(format, file, item)
	if err != nil {
		*warnings = append(*warnings, warning(format, file, err.Error()))

		return nil
	}

	*warnings = append(*warnings, warns...)

	return hooks
}

// hooksFromPathString resolves a plugin-relative hooks path; an escaping path
// is ignored with a warning.
func hooksFromPathString(format Format, root, file, rel string, warnings *[]string) []Hook {
	path, ok := localPath(root, rel)
	if !ok {
		*warnings = append(*warnings, warning(format, file, fmt.Sprintf("hook path %q escapes the package root; ignored", rel)))

		return nil
	}

	relPath, err := filepath.Rel(root, path)
	if err != nil {
		*warnings = append(*warnings, warning(format, file, fmt.Sprintf("hook path %q cannot be resolved; ignored", rel)))

		return nil
	}

	return hooksFromPath(format, root, filepath.ToSlash(relPath), warnings)
}

// warning renders the shared `origin: file: message` shape.
func warning(format Format, file, message string) string {
	return fmt.Sprintf("%s: %s: %s", format, file, message)
}

// readEntry reads path without following symlinks.
func readEntry(path string) ([]byte, entryKind, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, entryMissing, nil
	}

	if err != nil {
		return nil, entryMissing, err
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, entrySymlink, nil
	}

	if !info.Mode().IsRegular() {
		return nil, entryOther, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is resolved below the caller-provided package root
	if err != nil {
		return nil, entryMissing, err
	}

	// A leading UTF-8 BOM is stripped before decoding: hand-written documents
	// from Windows carry it and it never changes the meaning (decision T0.5 Q3).
	data = bytes.TrimPrefix(data, []byte("\ufeff"))

	return data, entryFile, nil
}

// entryKind classifies a payload path without following symlinks.
type entryKind int

const (
	// entryMissing is a path that does not exist.
	entryMissing entryKind = iota
	// entryFile is a regular file.
	entryFile
	// entrySymlink is a symlink, which is never followed.
	entrySymlink
	// entryOther is a directory or a special file.
	entryOther
)
