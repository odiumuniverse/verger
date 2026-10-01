package verger

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/secret"
)

// adapterFactories is the ten canonical host adapters this build can deliver
// to (DESIGN §2, U4). The list moved here from the CLI: a second front end that
// opens a client gets the same adapters over the same store, trash, ownership
// and secrets, so it never has to know how they are built. The constructors
// themselves stay in pkg/host.
func adapterFactories() []func(...host.Option) host.Host {
	return []func(...host.Option) host.Host{
		host.NewClaude, host.NewCodex, host.NewGemini, host.NewOmp, host.NewCursor,
		host.NewOpenCode, host.NewKilo, host.NewPi, host.NewAgy, host.NewDSH,
	}
}

// buildAdapters constructs every registered adapter over this client's store,
// trash, receipt-backed ownership and secrets. An injected adapter list wins:
// the caller that passed WithHosts keeps its own instances.
func (c *Client) buildAdapters() []host.Host {
	if injected := c.configured; len(injected) > 0 {
		return injected
	}

	options := []host.Option{
		host.WithStore(c.store),
		host.WithTrash(c.store.Trash()),
		host.WithOwnership(c.runOwner()),
	}

	if store, ok := c.secrets.(*secret.Store); ok {
		options = append(options, host.WithSecrets(store))
	}

	if c.logger.Log() != nil {
		options = append(options, host.WithLogger(c.logger))
	}

	if userHome, err := os.UserHomeDir(); err == nil {
		options = append(options, host.WithHome(userHome))
	}

	factories := adapterFactories()

	out := make([]host.Host, 0, len(factories))

	for _, factory := range factories {
		out = append(out, factory(options...))
	}

	return out
}

// HostFilter narrows the adapters an operation targets. Only and Except are the
// `--hosts` / `--except` flags; both name canonical host ids (U4).
type HostFilter struct {
	Only   []string
	Except []string
}

// Targets returns the detected adapters the filter selects. A known id with no
// adapter, or one the machine does not detect, is reported per host with the
// reason, never silently dropped.
func (c *Client) Targets(filter HostFilter) ([]host.Host, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}

	return c.selectAdapters(c.Hosts(), filter, func(adapter host.Host) bool {
		return adapter.Detect(userHome)
	})
}

// selectAdapters applies the filter to a caller-supplied adapter list.
//
// Targets is not enough for a verb that was handed its own list. A removal must
// keep the hosts that are NOT detected — uninstalling the agent you just
// removed is the ordinary case, not an error — so it cannot fall back to the
// detected set, and passing the list through unfiltered made `--hosts` a flag
// that printed a narrowed plan and then removed from every host anyway.
//
// present decides which adapters count as reachable, and is what keeps
// Targets' detection rule out of the removal path.
func (c *Client) selectAdapters(
	given []host.Host,
	filter HostFilter,
	present func(host.Host) bool,
) ([]host.Host, error) {
	only, err := hostSet(filter.Only, "--hosts")
	if err != nil {
		return nil, err
	}

	except, err := hostSet(filter.Except, "--except")
	if err != nil {
		return nil, err
	}

	var out []host.Host

	for _, adapter := range given {
		id := string(adapter.ID())

		if len(only) > 0 && !only[id] {
			continue
		}

		if except[id] || !present(adapter) {
			continue
		}

		out = append(out, adapter)
	}

	if len(out) == 0 && len(only) > 0 {
		return nil, &HostUnavailableError{
			Only:       only,
			Registered: AdapterIDs(given),
			Except:     except,
		}
	}

	return out, nil
}

// AdapterIDs indexes the registered adapters by host id: an id in this map is
// one this build can deliver to, whether or not the machine was detected.
func AdapterIDs(adapters []host.Host) map[string]bool {
	out := make(map[string]bool, len(adapters))

	for _, adapter := range adapters {
		out[string(adapter.ID())] = true
	}

	return out
}

// UnavailableHostsError is the verdict Targets returns when every requested host
// produced no adapter. It is exported so a front end can build the same message
// from its own registered list without matching on text.
func UnavailableHostsError(only, registered, except map[string]bool) error {
	return &HostUnavailableError{Only: only, Registered: registered, Except: except}
}

// HostUnavailableError reports that every requested host produced no adapter.
// Three different problems share this path and must not share one message: a
// host this build ships no adapter for, a host the caller excluded, and a
// registered adapter the machine does not detect (no CLI on PATH and no config
// under HOME — the contract every adapter's Detect implements).
type HostUnavailableError struct {
	Only       map[string]bool
	Registered map[string]bool
	Except     map[string]bool
}

// Error implements error.
func (e *HostUnavailableError) Error() string {
	parts := make([]string, 0, len(e.Only))

	for _, id := range slices.Sorted(maps.Keys(e.Only)) {
		switch {
		case !e.Registered[id]:
			parts = append(parts, id+" (no adapter in this build; planned for Ф2)")
		case e.Except[id]:
			parts = append(parts, id+" (excluded by --except)")
		default:
			parts = append(parts, id+" (adapter registered, but not detected: no CLI on PATH and no config under HOME)")
		}
	}

	if len(parts) == 0 {
		return "--hosts: no available adapter for " + strings.Join(slices.Sorted(maps.Keys(e.Only)), ", ")
	}

	return "--hosts: no available adapter for " + strings.Join(parts, "; ")
}

// UsageError reports a caller mistake: an unknown flag value, a malformed ref.
// It is the library's own typed error so a second front end can map it onto its
// own usage surface without parsing text.
type UsageError struct {
	Cause error
}

// Error implements error.
func (e *UsageError) Error() string {
	if e.Cause == nil {
		return "usage error"
	}

	return e.Cause.Error()
}

// Unwrap returns the underlying cause.
func (e *UsageError) Unwrap() error {
	return e.Cause
}

// hostSet parses one comma-separated host list.
func hostSet(values []string, flag string) (map[string]bool, error) {
	out := map[string]bool{}

	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}

			if !hostIDKnown(part) {
				return nil, &UsageError{Cause: fmt.Errorf("%s: unknown host %q", flag, part)}
			}

			out[part] = true
		}
	}

	return out, nil
}

// hostIDKnown reports whether id is one of the declared host ids.
func hostIDKnown(id string) bool {
	for _, known := range host.All() {
		if string(known) == id {
			return true
		}
	}

	return false
}

// runOwner returns the ownership source every adapter of this run shares, and
// builds it on first use so a caller that injected its own adapters still gets
// a layer BeginRun can clear.
func (c *Client) runOwner() *Ownership {
	if c.runOwnership != nil {
		return c.runOwnership
	}

	c.runOwnership = NewOwnership(c.Home().ReceiptsDir())

	return c.runOwnership
}
