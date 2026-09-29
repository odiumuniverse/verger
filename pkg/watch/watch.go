// Package watch turns filesystem changes under a host's paths into debounced
// batches.
//
// The engine reports *that a path under a host's watched set changed*: it does
// not compare bytes, does not filter the caller's own writes, and does not
// ignore editor swap files or other temporaries — a writer that swaps a file in
// (tmp + rename) produces exactly one batch, and everything else in the window
// is coalesced into the same one. The caller owns the filter: it knows which
// writes are its own and which files matter.
//
// fsnotify is not recursive, so directories are watched one level at a time:
// a new subdirectory is added when it appears, a removed one is dropped, and a
// path that does not exist yet is watched through its nearest existing parent
// and promoted when it is created.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce is the per-host window when no option overrides it.
const DefaultDebounce = 2 * time.Second

// Target is one host and the paths to watch for it.
type Target struct {
	Host  string
	Paths []string
}

// Event is one debounced batch of changed paths for one host.
type Event struct {
	Host  string
	Paths []string
	At    time.Time
}

// Option configures Run.
type Option func(*config)

// Logger is the diagnostics seam; nil means silent.
type Logger interface {
	Debugf(format string, args ...any)
	Warnf(format string, args ...any)
}

type config struct {
	debounce time.Duration
	clock    func() time.Time
	logger   Logger
	ready    chan<- struct{}
}

// WithDebounce sets the per-host coalescing window; zero and negative fall back
// to DefaultDebounce.
func WithDebounce(d time.Duration) Option {
	return func(c *config) { c.debounce = d }
}

// WithClock injects the clock that stamps Event.At. It does not drive the
// debounce timer: the window is real time, the stamp is the caller's.
func WithClock(clock func() time.Time) Option {
	return func(c *config) { c.clock = clock }
}

// WithLogger injects a diagnostics logger.
func WithLogger(l Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithReady receives one value when every initial watch is armed, so a caller
// never has to poll for the engine to become live.
func WithReady(ready chan<- struct{}) Option {
	return func(c *config) { c.ready = ready }
}

// DebounceFor resolves a requested window: zero and negative fall back to the
// default, because a zero window collapses every notification into its own
// batch — the behaviour this package exists to prevent.
func DebounceFor(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultDebounce
	}

	return d
}

// LimitError reports that the platform's watch limit is exhausted, with both
// remedies.
type LimitError struct {
	Path  string
	Op    string
	Cause error
}

// Error implements error.
func (e *LimitError) Error() string {
	return "watch: " + e.Op + " " + e.Path + ": the platform watch limit is exhausted (" +
		e.Cause.Error() + "); raise fs.inotify.max_user_watches on Linux, or narrow the target paths"
}

// Unwrap returns the underlying cause.
func (e *LimitError) Unwrap() error { return e.Cause }

// Run watches targets until ctx is cancelled. It returns nil on cancellation
// and a typed error (LimitError for a platform limit) when the watch cannot be
// established; a caller that stops reading out never wedges shutdown.
func Run(ctx context.Context, targets []Target, out chan<- Event, opts ...Option) error {
	cfg := config{debounce: DefaultDebounce, clock: time.Now}

	for _, opt := range opts {
		opt(&cfg)
	}

	cfg.debounce = DebounceFor(cfg.debounce)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	defer func() { _ = watcher.Close() }()

	r := &runner{
		addWatchFunc: watcher.Add,
		cfg:          cfg,
		out:          out,
		watcher:      watcher,
		owners:       map[string]map[string]bool{},
		pending:      map[string]map[string]bool{},
		due:          map[string]time.Time{},
	}

	if err := r.arm(targets); err != nil {
		return err
	}

	if cfg.ready != nil {
		cfg.ready <- struct{}{}
	}

	return r.loop(ctx)
}

// runner is one engine: fsnotify's channels, the watch ownership index and the
// per-host debounce state.
type runner struct {
	cfg     config
	out     chan<- Event
	watcher *fsnotify.Watcher

	// owners maps a watched path (directory or file) to the hosts that own it.
	owners map[string]map[string]bool
	// pending holds the paths accumulated for a host since its last batch.
	pending map[string]map[string]bool
	// due is the earliest time a host's batch may be flushed.
	due map[string]time.Time

	// addWatchFunc is how a path reaches the platform watcher. It is a field
	// rather than a direct watcher.Add call so a test can drive the watch-limit
	// path with a real ENOSPC: exhausting an actual inotify budget takes ~1M
	// registrations, while the typing is the thing worth proving.
	addWatchFunc func(path string) error

	timer *time.Timer
}

// arm registers every target path, watching the nearest existing parent of a
// path that does not exist yet.
func (r *runner) arm(targets []Target) error {
	for _, target := range targets {
		if target.Host == "" {
			return errors.New("watch: a target needs a host")
		}

		for _, path := range target.Paths {
			if err := r.armPath(target.Host, path); err != nil {
				return err
			}
		}
	}

	return nil
}

// armPath registers one path for one host.
func (r *runner) armPath(host, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("watch: resolve %s: %w", path, err)
	}

	if info, statErr := os.Stat(abs); statErr == nil {
		if !info.IsDir() {
			// A file target: watch it directly, so a write to it is reported.
			return r.addWatch(host, abs)
		}

		return r.addTree(host, abs)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("watch: stat %s: %w", abs, statErr)
	}

	// The path does not exist yet: watch the nearest existing ancestor.
	parent, ok := nearestExisting(abs)
	if !ok {
		return fmt.Errorf("watch: %s does not exist and has no watchable parent", abs)
	}

	r.cfg.debugf("watch: %s is not there yet; watching %s for it", abs, parent)

	return r.addWatch(host, parent)
}

// addTree watches a directory and every directory below it.
func (r *runner) addTree(host, dir string) error {
	if err := r.addWatch(host, dir); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("watch: read %s: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		if err := r.addTree(host, filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}

	return nil
}

// addWatch registers one path with fsnotify and records its owners.
func (r *runner) addWatch(host, path string) error {
	if owners, ok := r.owners[path]; ok {
		owners[host] = true

		return nil
	}

	if err := r.addWatchFunc(path); err != nil {
		if isLimit(err) {
			return &LimitError{Path: path, Op: "add", Cause: err}
		}

		return fmt.Errorf("watch: add %s: %w", path, err)
	}

	r.owners[path] = map[string]bool{host: true}
	r.cfg.debugf("watch: %s now watched for %s", path, host)

	return nil
}

// loop is the engine: fsnotify events in, debounced batches out, until the
// context is cancelled or the watcher closes.
func (r *runner) loop(ctx context.Context) error {
	defer r.stopTimer()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-r.watcher.Events:
			if !ok {
				return nil
			}

			if err := r.handle(ev); err != nil {
				return err
			}
		case err, ok := <-r.watcher.Errors:
			if !ok {
				return nil
			}

			if isLimit(err) {
				return &LimitError{Path: "", Op: "watch", Cause: err}
			}

			r.cfg.warnf("watch: %v", err)
		case <-r.timerC():
			if err := r.flush(ctx); err != nil {
				return err
			}
		}
	}
}

// handle folds one fsnotify event into the engine.
func (r *runner) handle(ev fsnotify.Event) error {
	hosts := r.hostsOf(ev.Name)

	if len(hosts) == 0 {
		return nil
	}

	if ev.Op&fsnotify.Create != 0 {
		if err := r.promote(hosts, ev.Name); err != nil {
			return err
		}
	}

	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		r.forget(ev.Name)
	}

	// A removed or renamed path is reported as a change, like any other: the
	// caller decides what a vanished path means for it.
	r.mark(hosts, ev.Name)

	return nil
}

// promote reacts to a created path: it adds a watch for a new directory and
// arms the watches a target path was waiting for.
//
// A created path that is ALREADY GONE is not a fault. The engine reacts to
// events, and by the time it gets to the event the file may have been removed
// again — an editor swap, a lock file, any write+unlink. Statting it and
// returning the error took the whole watcher down on ordinary filesystem
// traffic, so a vanished path is a no-op and every other stat failure is
// still reported.
func (r *runner) promote(hosts []string, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.cfg.debugf("watch: %s vanished before it could be promoted", path)

			return nil
		}

		return fmt.Errorf("watch: stat %s: %w", path, err)
	}

	if !info.IsDir() {
		return nil // a file needs no watch of its own
	}

	for _, host := range hosts {
		if err := r.addTree(host, path); err != nil {
			return err
		}
	}

	// The watch on a brand-new directory is armed only now, so anything
	// created inside it between the mkdir and this moment produced no event
	// and is lost. Re-scan the subtree and report what is already there: that
	// closes the one-step mkdir+write window that reacting to the parent's
	// Create event alone leaves open.
	r.markExisting(hosts, path)

	// A target path that was missing and has just appeared keeps the watch on
	// its nearest existing ancestor, armed when the path was first seen;
	// nothing more is needed, because that ancestor already covers it.
	r.cfg.debugf("watch: promoted %s", path)

	return nil
}

// markExisting reports the paths already present under root. It is the
// recovery half of promotion: a file written into a new directory before the
// directory's watch existed cannot produce an event, so the directory is read
// instead. The entries are reported like any other change and the per-host
// debounce coalesces them into the same batch as the directory itself.
func (r *runner) markExisting(hosts []string, root string) {
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A path that vanished mid-walk is the same benign race promote
			// just handled; anything else must not stop the walk.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		r.mark(hosts, path)

		return nil
	}); err != nil {
		r.cfg.debugf("watch: rescan of %s stopped early: %v", root, err)
	}
}

// forget drops the watches of a path that is gone, including any descendants.
func (r *runner) forget(path string) {
	for watched := range r.owners {
		if watched != path && !isUnder(watched, path) {
			continue
		}

		_ = r.watcher.Remove(watched)
		delete(r.owners, watched)
	}
}

// mark records a change for every owning host and (re)arms its window.
func (r *runner) mark(hosts []string, path string) {
	for _, host := range hosts {
		if r.pending[host] == nil {
			r.pending[host] = map[string]bool{}
		}

		r.pending[host][path] = true
		r.due[host] = time.Now().Add(r.cfg.debounce)
	}

	r.rearm()
}

// flush emits one batch per host whose window has closed.
func (r *runner) flush(ctx context.Context) error {
	now := time.Now()

	for host, deadline := range r.due {
		if deadline.After(now) {
			continue
		}

		paths := make([]string, 0, len(r.pending[host]))
		for path := range r.pending[host] {
			paths = append(paths, path)
		}

		slices.Sort(paths)
		delete(r.pending, host)
		delete(r.due, host)

		ev := Event{Host: host, Paths: paths, At: r.cfg.clock()}
		r.cfg.debugf("watch: batch for %s: %v", host, paths)

		select {
		case r.out <- ev:
		case <-ctx.Done():
			return nil
		}
	}

	r.rearm()

	return nil
}

// hostsOf attributes one path to the hosts that own a watch covering it.
func (r *runner) hostsOf(path string) []string {
	hosts := map[string]bool{}

	if owners, ok := r.owners[path]; ok {
		for host := range owners {
			hosts[host] = true
		}
	}

	// Walk up: a file inside a watched directory belongs to that directory's
	// hosts, and so does a path whose missing ancestor is watched.
	for dir := filepath.Dir(path); ; {
		if owners, ok := r.owners[dir]; ok {
			for host := range owners {
				hosts[host] = true
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}

		dir = parent
	}

	if len(hosts) == 0 {
		return nil
	}

	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}

	slices.Sort(out)

	return out
}

// rearm points the single timer at the earliest window that is still open.
func (r *runner) rearm() {
	earliest := time.Time{}

	for _, deadline := range r.due {
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}

	if earliest.IsZero() {
		r.stopTimer()

		return
	}

	wait := max(0, time.Until(earliest))

	if r.timer == nil {
		r.timer = time.NewTimer(wait)

		return
	}

	if !r.timer.Stop() {
		select {
		case <-r.timer.C:
		default:
		}
	}

	r.timer.Reset(wait)
}

// timerC is the timer channel of the shared timer; a nil timer never fires.
func (r *runner) timerC() <-chan time.Time {
	if r.timer == nil {
		return nil
	}

	return r.timer.C
}

// stopTimer stops the shared timer.
func (r *runner) stopTimer() {
	if r.timer == nil {
		return
	}

	if !r.timer.Stop() {
		select {
		case <-r.timer.C:
		default:
		}
	}

	r.timer = nil
}

// nearestExisting walks up from path until it finds something that exists.
func nearestExisting(path string) (string, bool) {
	for dir := path; ; {
		if _, err := os.Stat(dir); err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		dir = parent
	}
}

// isUnder reports whether path is below root.
func isUnder(path, root string) bool {
	return len(path) > len(root) && path[:len(root)] == root && path[len(root)] == filepath.Separator
}

// isLimit reports whether an fsnotify failure is the platform watch limit.
func isLimit(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// debugf logs when a logger is present.
func (c config) debugf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Debugf(format, args...)
	}
}

// warnf logs a warning when a logger is present.
func (c config) warnf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Warnf(format, args...)
	}
}
