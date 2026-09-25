package spec

import (
	"slices"
	"time"
)

// Event is one propagation event of DESIGN §5.7.
type Event string

// Propagation events.
const (
	EventInstall     Event = "install"
	EventRemove      Event = "remove"
	EventDisable     Event = "disable"
	EventEnable      Event = "enable"
	EventUpdate      Event = "update"
	EventAdopt       Event = "adopt"
	EventMarketplace Event = "marketplace"
)

// events lists every propagation event in schema order. It is a fixed-size
// array: a compile-time read-only table (decision T0.5 N2).
var events = [...]Event{
	EventInstall,
	EventRemove,
	EventDisable,
	EventEnable,
	EventUpdate,
	EventAdopt,
	EventMarketplace,
}

// eventKeys are the TOML keys of the propagation events; a fixed-size array
// like events (decision T0.5 N2).
var eventKeys = [...]string{
	"install", "remove", "disable", "enable", "update", "adopt", "marketplace",
}

// Valid reports whether e is a documented propagation event.
func (e Event) Valid() bool {
	return slices.Contains(events[:], e)
}

// Mode is one propagation answer: to every host, only the origin host or ask
// the user.
type Mode string

// Propagation modes.
const (
	ModeAll    Mode = "all"
	ModeOrigin Mode = "origin"
	ModeAsk    Mode = "ask"
)

// Propagate is a propagation policy: one mode per event plus optional per-kind
// and per-host override tables (DESIGN §5.7). An unset mode is empty and
// resolves to ModeAll.
type Propagate struct {
	Install     Mode `toml:"install,omitempty"`
	Remove      Mode `toml:"remove,omitempty"`
	Disable     Mode `toml:"disable,omitempty"`
	Enable      Mode `toml:"enable,omitempty"`
	Update      Mode `toml:"update,omitempty"`
	Adopt       Mode `toml:"adopt,omitempty"`
	Marketplace Mode `toml:"marketplace,omitempty"`

	Kind map[string]Propagate `toml:"kind,omitempty"`
	Host map[string]Propagate `toml:"host,omitempty"`

	raw map[string]any
}

// Mode returns the mode set for one event, or "" when unset.
func (p *Propagate) Mode(ev Event) Mode {
	if p == nil {
		return ""
	}

	switch ev {
	case EventInstall:
		return p.Install
	case EventRemove:
		return p.Remove
	case EventDisable:
		return p.Disable
	case EventEnable:
		return p.Enable
	case EventUpdate:
		return p.Update
	case EventAdopt:
		return p.Adopt
	case EventMarketplace:
		return p.Marketplace
	default:
		return ""
	}
}

func (m Mode) valid() bool {
	switch m {
	case ModeAll, ModeOrigin, ModeAsk:
		return true
	default:
		return false
	}
}

func (h HooksMode) valid() bool {
	switch h {
	case HooksAsk, HooksYes, HooksNo:
		return true
	default:
		return false
	}
}

// EffectiveMode resolves one propagation event through global → kind → host →
// package, the narrowest set value winning; unset everywhere means ModeAll.
// Nil receivers and a nil package are valid.
func (s *Spec) EffectiveMode(ev Event, kind, host string, pkg *Package) Mode {
	mode := ModeAll

	if s != nil {
		if set := s.Propagate.Mode(ev); set != "" {
			mode = set
		}

		kindPolicy := s.Propagate.Kind[kind]
		if set := kindPolicy.Mode(ev); set != "" {
			mode = set
		}

		hostPolicy := s.Propagate.Host[host]
		if set := hostPolicy.Mode(ev); set != "" {
			mode = set
		}
	}

	if pkg != nil && pkg.Propagate != nil {
		if set := pkg.Propagate.Mode(ev); set != "" {
			mode = set
		}
	}

	return mode
}

// Cooldown returns the configured upstream update delay, or DefaultCooldown
// when unset.
func (s *Spec) Cooldown() time.Duration {
	if s == nil || s.Defaults.Cooldown == 0 {
		return DefaultCooldown
	}

	return s.Defaults.Cooldown.Duration()
}

// HooksMode returns the configured hooks consent default, or HooksAsk when
// unset.
func (s *Spec) HooksMode() HooksMode {
	if s == nil || s.Defaults.Hooks == "" {
		return HooksAsk
	}

	return s.Defaults.Hooks
}
