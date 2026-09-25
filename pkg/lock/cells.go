package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/digest"
)

// Scopes of a cell.
const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// Strategy is how a package is delivered into a host.
type Strategy string

// Delivery strategies.
const (
	StrategyNative   Strategy = "native"
	StrategySynth    Strategy = "synth"
	StrategyLoose    Strategy = "loose"
	StrategySilenced Strategy = "silenced"
)

// cellKeys are the JSON fields Cell declares; everything else is kept as an
// unknown field for round-trip.
var cellKeys = [...]string{
	"package", "host", "scope", "version", "strategy",
	"reason", "caps_hash", "digest", "source", "ref", "updated_at",
}

// Cell is the exact state of one (package, host, scope): which version landed,
// through which strategy and with which digests.
type Cell struct {
	Package   string      `json:"package"`
	Host      string      `json:"host"`
	Scope     string      `json:"scope"` // user|project
	Version   string      `json:"version"`
	Strategy  Strategy    `json:"strategy"`
	Reason    string      `json:"reason,omitempty"` // required when StrategySilenced
	CapsHash  string      `json:"caps_hash,omitempty"`
	Digest    digest.Hash `json:"digest,omitempty"` // payload tree digest
	Source    string      `json:"source,omitempty"`
	Ref       string      `json:"ref,omitempty"` // git commit / archive pin
	UpdatedAt time.Time   `json:"updated_at,omitzero"`

	extra map[string]json.RawMessage
}

// ValidateCell reports whether a cell may enter the lock: non-empty package and
// host, a known scope and strategy, a reason for silenced cells, a version for
// every other strategy and a valid digest when one is set.
func ValidateCell(c Cell) error {
	if c.Package == "" {
		return invalidCell(c, errors.New("empty package"))
	}

	if c.Host == "" {
		return invalidCell(c, errors.New("empty host"))
	}

	switch c.Scope {
	case ScopeUser, ScopeProject:
	default:
		return invalidCell(c, fmt.Errorf("unknown scope %q", c.Scope))
	}

	switch c.Strategy {
	case StrategyNative, StrategySynth, StrategyLoose:
		if c.Version == "" {
			return invalidCell(c, errors.New("non-silenced cell needs a version"))
		}
	case StrategySilenced:
		if c.Reason == "" {
			return invalidCell(c, errors.New("silenced cell needs a reason"))
		}
	default:
		return invalidCell(c, fmt.Errorf("unknown strategy %q", c.Strategy))
	}

	if c.Digest != "" {
		if _, err := digest.Parse(c.Digest.String()); err != nil {
			return invalidCell(c, err)
		}
	}

	return nil
}

func invalidCell(c Cell, cause error) error {
	return &InvalidCellError{Package: c.Package, Host: c.Host, Scope: c.Scope, Cause: cause}
}

// MarshalJSON encodes the cell with snake_case keys in deterministic
// (alphabetical) order and splices back the unknown fields. The declared tags
// (omitempty/omitzero) decide which known fields appear.
func (c Cell) MarshalJSON() ([]byte, error) {
	type plain Cell

	known, err := encodeJSON(plain(c))
	if err != nil {
		return nil, &InvalidCellError{Package: c.Package, Host: c.Host, Scope: c.Scope, Cause: err}
	}

	return mergeExtras(known, cellKeys[:], c.extra)
}

// UnmarshalJSON decodes the cell and keeps unknown fields for round-trip.
func (c *Cell) UnmarshalJSON(data []byte) error {
	type plain Cell

	var parsed plain

	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}

	*c = Cell(parsed)
	c.extra = dropKeys(all, cellKeys[:])

	return nil
}

// clone returns a copy carrying the unknown fields.
func (c Cell) clone() Cell {
	out := c
	out.extra = c.extra

	return out
}

// mergeCellExtras overlays incoming extras on top of existing ones.
func mergeCellExtras(existing, incoming map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(existing)+len(incoming))

	maps.Copy(out, existing)
	maps.Copy(out, incoming)

	if len(out) == 0 {
		return nil
	}

	return out
}

// dropKeys removes the named keys (case-insensitively) from a raw object and
// returns the rest, or nil when nothing is left.
func dropKeys(fields map[string]json.RawMessage, names []string) map[string]json.RawMessage {
	for key := range fields {
		for _, name := range names {
			if strings.EqualFold(key, name) {
				delete(fields, key)

				break
			}
		}
	}

	if len(fields) == 0 {
		return nil
	}

	return fields
}
