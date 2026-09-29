package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/spec"
)

// newPropagateCmd builds `verger propagate`.
func newPropagateCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "propagate",
		Short: "Show or set the propagation policy (§5.7)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show the effective propagation policy",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runPropagateShow(cmd.Context())
		},
	}

	var (
		kind   string
		hostID string
	)

	setCmd := &cobra.Command{
		Use:   "set <event> <value>",
		Short: "Set one propagation rule (value: all|origin|ask)",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runPropagateSet(cmd.Context(), args[0], args[1], kind, hostID)
		},
	}

	setCmd.Flags().StringVar(&kind, "kind", "", "scope the rule to one component kind")
	setCmd.Flags().StringVar(&hostID, "host", "", "scope the rule to one host")
	// A propagation rule is written as given; nothing asks first.
	addDryRunFlag(setCmd, a)

	showCmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")
	setCmd.Flags().BoolVar(&a.projectFlag, "project", false, "use the project scope")

	cmd.AddCommand(showCmd, setCmd)

	return cmd
}

// propagateDoc is the stable JSON shape of the policy.
type propagateDoc struct {
	Schema schemaRef                    `json:"schema"`
	Rules  map[string]string            `json:"rules"`
	Kind   map[string]map[string]string `json:"kind,omitempty"`
	Host   map[string]map[string]string `json:"host,omitempty"`
}

// propagationEvents lists every §5.7 event in the display order.
var propagationEvents = [...]spec.Event{
	spec.EventInstall, spec.EventRemove, spec.EventDisable, spec.EventEnable,
	spec.EventUpdate, spec.EventAdopt, spec.EventMarketplace,
}

// policyModes renders the set modes of one policy.
func policyModes(policy spec.Propagate) map[string]string {
	out := map[string]string{}

	for _, event := range propagationEvents {
		if mode := propagateMode(policy, event); mode != "" {
			out[string(event)] = string(mode)
		}
	}

	return out
}

// runPropagateShow prints the effective policy.
func (a *app) runPropagateShow(ctx context.Context) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	doc, _, err := loadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	out := propagateDoc{
		Schema: schemaOf(schemaPropagate),
		Rules:  policyModes(doc.Propagate),
		Kind:   map[string]map[string]string{},
		Host:   map[string]map[string]string{},
	}

	for _, event := range propagationEvents {
		if _, ok := out.Rules[string(event)]; !ok {
			out.Rules[string(event)] = string(spec.ModeAll)
		}
	}

	for name, policy := range doc.Propagate.Kind {
		out.Kind[name] = policyModes(policy)
	}

	for name, policy := range doc.Propagate.Host {
		out.Host[name] = policyModes(policy)
	}

	if a.jsonOut {
		return a.printJSON(out)
	}

	return a.printPropagate(out)
}

// printPropagate renders one policy document as a table: the flat rules first,
// then the kind/ and host/ scopes.
func (a *app) printPropagate(doc propagateDoc) error {
	names := make([]string, 0, len(doc.Rules))

	for name := range doc.Rules {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if _, err := fmt.Fprintf(a.out, "%-12s %s\n", name, doc.Rules[name]); err != nil {
			return err
		}
	}

	for _, scoped := range []struct {
		prefix   string
		policies map[string]map[string]string
	}{
		{prefix: "kind/", policies: doc.Kind},
		{prefix: "host/", policies: doc.Host},
	} {
		for _, name := range slices.Sorted(maps.Keys(scoped.policies)) {
			for _, event := range propagationEvents {
				if mode, ok := scoped.policies[name][string(event)]; ok {
					if _, err := fmt.Fprintf(a.out, "%-12s %-10s %s\n", scoped.prefix+name, event, mode); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// propagateMode reads one event mode of a policy.
func propagateMode(policy spec.Propagate, event spec.Event) spec.Mode {
	switch event {
	case spec.EventInstall:
		return policy.Install
	case spec.EventRemove:
		return policy.Remove
	case spec.EventDisable:
		return policy.Disable
	case spec.EventEnable:
		return policy.Enable
	case spec.EventUpdate:
		return policy.Update
	case spec.EventAdopt:
		return policy.Adopt
	case spec.EventMarketplace:
		return policy.Marketplace
	default:
		return ""
	}
}

// setPropagateMode stores one event mode of a policy.
func setPropagateMode(policy *spec.Propagate, event spec.Event, mode spec.Mode) {
	switch event {
	case spec.EventInstall:
		policy.Install = mode
	case spec.EventRemove:
		policy.Remove = mode
	case spec.EventDisable:
		policy.Disable = mode
	case spec.EventEnable:
		policy.Enable = mode
	case spec.EventUpdate:
		policy.Update = mode
	case spec.EventAdopt:
		policy.Adopt = mode
	case spec.EventMarketplace:
		policy.Marketplace = mode
	}
}

// runPropagateSet writes one propagation rule into the spec.
func (a *app) runPropagateSet(ctx context.Context, eventArg, value, kind, hostID string) error {
	event := spec.Event(eventArg)

	if !event.Valid() {
		return &UsageError{Cause: fmt.Errorf("unknown event %q", eventArg)}
	}

	mode := spec.Mode(value)

	switch mode {
	case spec.ModeAll, spec.ModeOrigin, spec.ModeAsk:
	default:
		return &UsageError{Cause: fmt.Errorf("unknown value %q (all|origin|ask)", value)}
	}

	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	if err := a.requireTrust(client, paths); err != nil {
		return err
	}

	doc, _, err := loadSpec(paths.SpecPath)
	if err != nil {
		return err
	}

	policy := &doc.Propagate

	if kind != "" || hostID != "" {
		nested := scopedPolicy(policy, kind, hostID)
		setPropagateMode(&nested, event, mode)
		storeScopedPolicy(policy, kind, hostID, nested)
	} else {
		setPropagateMode(policy, event, mode)
	}

	if a.dryRun {
		_, err := fmt.Fprintf(a.out, "propagate: would set %s=%s\n", event, mode)

		return err
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := saveSpec(paths.SpecPath, doc); err != nil {
		return err
	}

	_, err = fmt.Fprintf(a.out, "propagate %s=%s\n", event, mode)

	return err
}

// scopedPolicy returns the kind- or host-scoped policy of one spec.
func scopedPolicy(policy *spec.Propagate, kind, hostID string) spec.Propagate {
	if kind != "" {
		return policy.Kind[kind]
	}

	return policy.Host[hostID]
}

// storeScopedPolicy writes a scoped policy back after setPropagateMode
// mutated the copy returned by scopedPolicy.
func storeScopedPolicy(policy *spec.Propagate, kind, hostID string, nested spec.Propagate) {
	if kind != "" {
		if policy.Kind == nil {
			policy.Kind = map[string]spec.Propagate{}
		}

		policy.Kind[kind] = nested

		return
	}

	if policy.Host == nil {
		policy.Host = map[string]spec.Propagate{}
	}

	policy.Host[hostID] = nested
}
