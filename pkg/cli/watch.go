package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// watchOwner is the lease owner `verger watch` claims when the caller names
// none. It must be stable across restarts — the lease is what stops two
// watchers writing at once (D19), so an owner that changed every run would
// never exclude anything.
//
// It is the facade's own constant, not a literal here: the owner vocabulary
// is closed, and a third spelling in this file was rejected by the very
// check it was meant to satisfy.
const watchOwner = verger.LeaseOwnerVerger

// newWatchCmd builds `verger watch`.
func newWatchCmd(a *app) *cobra.Command {
	var (
		owner    string
		debounce time.Duration
	)

	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Reconcile the scope whenever a host's files change",
		Long: "Reconcile in the foreground until interrupted.\n\n" +
			"verger watch takes this home's watch lease, so two watchers can never\n" +
			"write at once, and reconciles a host whenever one of its files changes.\n" +
			"SIGINT or SIGTERM stops it and releases the lease.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runWatch(cmd.Context(), owner, debounce)
		},
	}

	cmd.Flags().StringVar(&owner, "owner", watchOwner, "lease owner name (stable across restarts)")
	cmd.Flags().DurationVar(&debounce, "debounce", verger.DefaultWatchDebounce, "per-host debounce window")
	cmd.Flags().BoolVar(&a.projectFlag, "project", false, "watch the project scope")
	cmd.Flags().StringSliceVar(&a.hostsFlag, "hosts", nil, "only these hosts (comma separated)")
	cmd.Flags().StringSliceVar(&a.exceptFlag, "except", nil, "exclude these hosts")
	cmd.Flags().BoolVar(&a.jsonOut, "json", false, "print one JSON document per reconcile")

	return cmd
}

// runWatch runs one foreground watch and translates its outcome into an exit
// verdict.
//
// Two rules matter here and both are about not lying to a script:
//
//   - An interrupt is a success. The context is cancelled by SIGINT and
//     SIGTERM (see Execute), and a watcher the user stopped must not print a
//     failure or exit non-zero — otherwise every `Ctrl-C` looks like a crash.
//   - A busy lease is a conflict, not a crash. The facade returns
//     *home.LeaseHeldError naming the owner and pid, and the exit classifier
//     already maps it to 3.
func (a *app) runWatch(ctx context.Context, owner string, debounce time.Duration) error {
	client, err := a.open(ctx)
	if err != nil {
		return err
	}

	paths, err := a.paths(client)
	if err != nil {
		return err
	}

	opts := verger.WatchOptions{
		Paths:    paths,
		Owner:    owner,
		Filter:   verger.HostFilter{Only: a.hostsFlag, Except: a.exceptFlag},
		Debounce: debounce,
		DryRun:   a.dryRun,
		Hooks:    verger.HooksAsk,
	}

	events := make(chan verger.Event, watchEventBuffer)
	opts.Events = events

	// Progress goes to stderr so `verger watch > log` still captures the
	// watcher's own output on stdout and nothing interleaves with it.
	done := make(chan error, 1)

	go func() { done <- client.Watch(ctx, opts) }()

	for {
		select {
		case <-ctx.Done():
			// The facade's own run is already cancelled with this context;
			// wait briefly for it to release the lease rather than exiting
			// while the lease file is still held.
			return a.drainWatch(ctx, done, events)

		case event := <-events:
			a.printWatchEvent(event)

		case watchErr := <-done:
			if watchErr == nil || errors.Is(watchErr, context.Canceled) || errors.Is(watchErr, context.DeadlineExceeded) {
				return nil
			}

			return watchErr
		}
	}
}

// drainWatch lets a cancelled watch finish so it releases the lease, within
// the shutdown budget. Returning early would leave a lease naming a process
// that no longer exists, and the next watcher would refuse to start.
func (a *app) drainWatch(ctx context.Context, done <-chan error, events <-chan verger.Event) error {
	timer := time.NewTimer(watchShutdownBudget)
	defer timer.Stop()

	for {
		select {
		case watchErr := <-done:
			_ = watchErr

			return nil

		case <-events:
			// Drain progress so a reconcile in flight does not block on a
			// reader that has gone away.
		case <-timer.C:
			return nil
		}
	}
}

// watchShutdownBudget is how long a cancelled watch may take to release its
// lease before the CLI stops waiting. It bounds Ctrl-C at two seconds.
const watchShutdownBudget = 2 * time.Second

// watchEventBuffer is how many reconciles may queue for the progress writer.
const watchEventBuffer = 32

// printWatchEvent renders one reconcile the facade reported.
func (a *app) printWatchEvent(event verger.Event) {
	if a.jsonOut {
		fmt.Fprintln(a.out, `{"event":`+quoteJSON(string(event.Kind))+`,"message":`+quoteJSON(event.Message)+`}`)

		return
	}

	fmt.Fprintln(a.errW, "watch:", event.Message)
}

// quoteJSON renders a string as a JSON scalar, quotes included.
func quoteJSON(s string) string {
	quoted, err := json.Marshal(s)
	if err != nil {
		return `""`
	}

	return string(quoted)
}
