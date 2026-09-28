package verger

import (
	"maps"
	"time"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/spec"
)

// Feature switches (U2: every behaviour on by default, each switchable off).
//
// The switches are read from the spec — `verger.toml` — so they survive a
// restart and are visible to the user; the same values are also accepted as
// options so a front end can override them for one run. Nothing here is
// opt-in: a missing key means on.

// Switches are the effective settings of one run, resolved from the spec and the
// run's options in that order.
type Switches struct {
	// Hooks is the effective hooks consent mode.
	Hooks HooksMode
	// Cooldown is how long a failed host is left alone before it is retried; the
	// zero value means "no cooldown".
	Cooldown time.Duration
	// Host holds the per-host switches, keyed by host id.
	Host map[host.ID]spec.HostSettings
}

// SwitchOptions override the spec for one run.
type SwitchOptions struct {
	// Hooks wins over the spec default when set to something other than
	// HooksAsk.
	Hooks HooksMode
	// Cooldown wins over the spec default when non-zero.
	Cooldown time.Duration
	// Hosts overrides the spec's per-host table for the ids it names.
	Hosts map[host.ID]spec.HostSettings
}

// ResolveSwitches reads the effective switches of one scope: the spec first,
// then the run's overrides.
func ResolveSwitches(paths Paths, opts SwitchOptions) (Switches, error) {
	doc, _, err := LoadSpec(paths.SpecPath)
	if err != nil {
		return Switches{}, err
	}

	out := Switches{
		Hooks:    LibraryHooksMode(doc.Defaults.Hooks),
		Cooldown: doc.Defaults.Cooldown.Duration(),
		Host:     map[host.ID]spec.HostSettings{},
	}

	for id, settings := range doc.Hosts {
		out.Host[host.ID(id)] = settings
	}

	maps.Copy(out.Host, opts.Hosts)

	if opts.Hooks != HooksAsk {
		out.Hooks = opts.Hooks
	}

	if opts.Cooldown > 0 {
		out.Cooldown = opts.Cooldown
	}

	return out, nil
}

// Enabled reports whether one host takes part in a run at all.
func (s Switches) Enabled(id host.ID) bool {
	settings, ok := s.Host[id]
	if !ok {
		return true
	}

	return settings.EnabledOrDefault()
}

// HooksOn reports whether hooks may be delivered to one host.
func (s Switches) HooksOn(id host.ID) bool {
	settings, ok := s.Host[id]
	if !ok {
		return true
	}

	return settings.HooksOn()
}

// RuntimeOn reports whether one host's runtime channel is on.
func (s Switches) RuntimeOn(id host.ID) bool {
	settings, ok := s.Host[id]
	if !ok {
		return true
	}

	return settings.RuntimeOn()
}

// Filter drops the adapters the switches turn off, and the hosts a package's own
// `except` list names. It is the one place both switches are applied, so a
// caller cannot forget one.
func (s Switches) Filter(adapters []host.Host, except []string) []host.Host {
	skip := make(map[host.ID]bool, len(except))

	for _, id := range except {
		skip[host.ID(id)] = true
	}

	out := make([]host.Host, 0, len(adapters))

	for _, adapter := range adapters {
		if skip[adapter.ID()] || !s.Enabled(adapter.ID()) {
			continue
		}

		out = append(out, adapter)
	}

	return out
}

// ExceptFor returns the host ids one spec package excludes. A package that names
// none excludes nothing, so the default is every host (U2).
func ExceptFor(doc *spec.Spec, id string) []string {
	entry, ok := SpecPackage(doc, id)
	if !ok {
		return nil
	}

	return entry.Except
}

// switchesFor resolves the effective switches of one run: the spec's table and
// defaults, with the run's overrides on top.
func (c *Client) switchesFor(paths Paths, overrides *SwitchOptions) (Switches, error) {
	var opts SwitchOptions
	if overrides != nil {
		opts = *overrides
	}

	return ResolveSwitches(paths, opts)
}
