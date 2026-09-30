package runtime

import (
	"encoding/json"
	"fmt"
)

// Table is the shared hook event table (table/events.json): one row per
// canonical event, one column per dialect. The same bytes are imported by the
// bundle, so Go and the runtime cannot drift.
type Table struct {
	Dialects      []string            `json:"dialects"`
	Events        map[string]EventRow `json:"events"`
	V2Namespaces  map[string]string   `json:"v2Namespaces"`
	Tools         map[string]string   `json:"tools"`
	Aliases       map[string]string   `json:"aliases"`
	Uncategorised string              `json:"uncategorised"`
}

// EventRow is one canonical event across the dialects; an empty dialect name
// means the dialect has no verified event for it and the hook is never
// registered there.
type EventRow struct {
	V1       string `json:"v1"`
	V2       string `json:"v2"`
	Pi       string `json:"pi"`
	Blocking bool   `json:"blocking"`
}

// ParseTable decodes one event table document.
func ParseTable(data []byte) (Table, error) {
	var table Table

	if err := json.Unmarshal(data, &table); err != nil {
		return Table{}, fmt.Errorf("parse runtime table: %w", err)
	}

	return table, nil
}

// LoadTable parses the embedded event table.
func LoadTable() (Table, error) {
	data, err := TableBytes()
	if err != nil {
		return Table{}, err
	}

	return ParseTable(data)
}

// HookKey returns the host hook key of one canonical event in one dialect; ok
// is false when the dialect has no verified event for it.
func (t Table) HookKey(dialect, canon string) (string, bool) {
	row, ok := t.Events[canon]
	if !ok {
		return "", false
	}

	var key string

	switch dialect {
	case "v1":
		key = row.V1
	case "v2":
		key = row.V2
	case "pi":
		key = row.Pi
	default:
		return "", false
	}

	return key, key != ""
}

// BlockingKeys lists the hook keys a dialect registers for the blocking
// canonical events: the keys the shim's inline deny fallback must cover when
// the runtime cannot be imported.
func (t Table) BlockingKeys(dialect string) []string {
	var keys []string

	for _, canon := range t.CanonicalEvents() {
		if !t.Events[canon].Blocking {
			continue
		}

		if key, ok := t.HookKey(dialect, canon); ok {
			keys = append(keys, key)
		}
	}

	return sortedUnique(keys)
}

// Keys returns every host hook key of a dialect, blocking or not. The shim
// registers all of them: a hook on a non-blocking event — a session start, a
// turn end — is a hook the host is expected to run, and registering only the
// blocking ones left those hooks with no handler at all.
func (t Table) Keys(dialect string) []string {
	var keys []string

	for _, canon := range t.CanonicalEvents() {
		if key, ok := t.HookKey(dialect, canon); ok {
			keys = append(keys, key)
		}
	}

	return sortedUnique(keys)
}

// CanonicalEvents lists the canonical events in table order.
func (t Table) CanonicalEvents() []string {
	events := make([]string, 0, len(t.Events))

	for canon := range t.Events {
		events = append(events, canon)
	}

	return sortedUnique(events)
}
