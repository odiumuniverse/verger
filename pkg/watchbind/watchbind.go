// Package watchbind binds the facade's watch seam to the real engine in
// pkg/watch.
//
// The seam exists so pkg/verger never imports the engine: the facade is the
// embedding library, and beadle — not verger — decides which engine a build
// carries. That is right, but it left the seam unwired: nothing called
// verger.SetWatchEngine, so Client.Watch answered "not available" in every
// binary including verger's own.
//
// This package is the one place both sides can call. It translates the
// facade's shapes into the engine's, installs the result, and is safe to call
// more than once.
package watchbind

import (
	"context"
	"time"

	"github.com/odiumuniverse/verger/pkg/verger"
	"github.com/odiumuniverse/verger/pkg/watch"
)

// Install binds pkg/watch to the facade seam and returns. It is idempotent:
// calling it again re-installs the same engine, so a main that wants to be
// explicit and a test that wants the real engine can both call it without
// coordinating.
//
// Beadle calls this too, so the same facade binary gets a live watcher
// whichever side wires it.
func Install() {
	verger.SetWatchEngine(Run)
}

// Run is the engine, translated. It satisfies verger.WatchEngine and is
// exported so a caller that wants to compose it — a test engine that falls
// back to it, or a front end that needs the raw error — can use it directly.
func Run(ctx context.Context, targets []verger.WatchTarget, out chan<- verger.WatchEvent, opts ...verger.WatchOption) error {
	engineTargets := make([]watch.Target, 0, len(targets))
	for _, target := range targets {
		engineTargets = append(engineTargets, watch.Target{Host: target.Host, Paths: target.Paths})
	}

	events := make(chan watch.Event, watchBuffer)

	done := make(chan error, 1)

	go func() { done <- watch.Run(ctx, engineTargets, events, engineOptions(opts)...) }()

	// Forward until the engine stops or the caller stops reading out. The
	// engine's contract is that a caller that never reads does not wedge
	// shutdown, so a short-lived forwarder is the whole translation.
	defer close(events)

	for {
		select {
		case <-ctx.Done():
			return nil

		case err := <-done:
			return err

		case event, ok := <-events:
			if !ok {
				return nil
			}

			out <- verger.WatchEvent{Host: event.Host, Paths: event.Paths, At: event.At}
		}
	}
}

// watchBuffer matches the facade's own queue depth, so the engine never
// blocks the forwarder on a burst.
const watchBuffer = 32

// engineOptions turns the facade's opaque options into the engine's own.
func engineOptions(opts []verger.WatchOption) []watch.Option {
	cfg := verger.FoldWatchOptions(verger.NewWatchConfig(), opts...)

	if cfg.Debounce <= 0 {
		return nil
	}

	return []watch.Option{watch.WithDebounce(watch.DebounceFor(cfg.Debounce))}
}

// DebounceOf reports the debounce a set of options carries, and whether any of
// them set one. It is the same read ApplyOptions performs, for a caller that
// wants the value rather than the engine option.
func DebounceOf(opts ...verger.WatchOption) (time.Duration, bool) {
	cfg := verger.FoldWatchOptions(verger.NewWatchConfig(), opts...)

	return cfg.Debounce, cfg.Debounce > 0
}
