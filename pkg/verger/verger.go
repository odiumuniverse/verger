// Package verger is the embedding facade: beadle and CLIs open a Client and ask
// it for machine paths and, in later phases, plan/apply/status/watch. The Ф0
// surface is Open/Close, the WithHome/WithLogger/WithStore options and
// Machine(); secrets (T1.1: WithSecrets, else <home>/state/secrets.json) and
// host adapters (T1.6: WithHosts) arrived additively. A missing secrets file is
// an empty store; an unreadable or corrupt one fails with
// *OpenError{Option: OptionSecrets}. Client.Secrets is never nil after a
// successful Open. Plan/apply/status/watch APIs arrive with their owning tasks
// and are deliberately absent here.
package verger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

// SecretsStore is the secrets interface the client needs; *secret.Store
// implements it, and so can beadle's own store.
type SecretsStore interface {
	Get(string) (string, bool)
	Set(string, string)
	Delete(string) bool
	Save() error
	Names() []string
	Changed() bool
	Path() string
}

// Client is an open facade over one resolved home and machine store.
type Client struct {
	home    *home.Home
	store   *store.Store
	secrets SecretsStore
	logger  embedlog.Logger

	// configured is the adapter list a caller injected with WithHosts. An empty
	// list means "build the registered adapters" (DESIGN §9.1), so a front end
	// that only calls Open still gets all ten over this client's store.
	configured []host.Host
	// adapters is the built list, memoized so Targets and every operation see
	// the same instances (an adapter is stateless, but the receipt-backed
	// ownership source it carries is one receipt store).
	adapters []host.Host
	// now is the clock every "when did this last happen" decision reads. It is
	// a field rather than a call to time.Now so that a window - the update
	// cooldown - is something a test can place, not something a test has to
	// wait for.
	now    func() time.Time
	mu     sync.Mutex
	closed bool
}

// config carries the option values of one Open call.
type config struct {
	homePath          string
	homeSet           bool
	storeRoot         string
	storeSet          bool
	secrets           SecretsStore
	secretsSet        bool
	logger            embedlog.Logger
	loggerSet         bool
	hosts             []host.Host
	hostsSet          bool
	trashRetention    time.Duration
	trashRetentionSet bool
	now               func() time.Time
}

// Option configures Open.
type Option func(*config)

// WithHome overrides home discovery (DESIGN §3.2); `~` is expanded. Empty,
// whitespace-only and relative paths are rejected by Open with
// *OpenError{Option: OptionHome}, never at option-construction time.
func WithHome(path string) Option {
	return func(c *config) {
		c.homePath = path
		c.homeSet = true
	}
}

// WithLogger sets the embedlog logger used for diagnostics.
func WithLogger(log embedlog.Logger) Option {
	return func(c *config) {
		c.logger = log
		c.loggerSet = true
	}
}

// WithTrashRetention sets how long a replaced file stays recoverable in the
// store's trash. A value <= 0 is ignored rather than meaning "keep nothing":
// a zero window makes every replaced artifact unrecoverable the moment a
// purge runs, and a caller who meant "use the default" said so by not
// calling this at all.
func WithTrashRetention(d time.Duration) Option {
	return func(c *config) {
		if d <= 0 {
			return
		}

		c.trashRetention = d
		c.trashRetentionSet = true
	}
}

// WithClock sets the time the facade reads when a decision depends on "now":
// the update cooldown, above all. The zero value is time.Now.
//
// It exists because a throttle is the one thing in the library that cannot be
// tested by running the test: a cooldown of 24h asserted against a real clock
// either passes instantly or needs a day. A caller that embeds verger - beadle
// is the one that matters - can pass the same clock it uses for everything
// else, so "is this inside the window" is a question with an answer.
func WithClock(now func() time.Time) Option {
	return func(c *config) {
		if now == nil {
			return
		}

		c.now = now
	}
}

// WithStore overrides the machine store root (DESIGN §3.1); `~` is expanded.
// Empty and whitespace-only roots are rejected by Open with
// *OpenError{Option: OptionStore}, never at option-construction time.
func WithStore(root string) Option {
	return func(c *config) {
		c.storeRoot = root
		c.storeSet = true
	}
}

// WithSecrets injects the secrets store (DESIGN §9.1); beadle passes its own
// keyring-backed store. A nil store is rejected by Open with
// *OpenError{Option: OptionSecrets}, never at option-construction time.
func WithSecrets(s SecretsStore) Option {
	return func(c *config) {
		c.secrets = s
		c.secretsSet = true
	}
}

// WithHosts overrides host discovery with an explicit adapter list; a nil
// element is rejected by Open with *OpenError{Option: OptionHosts}. The
// adapters must be built over the store the client opens — the same store as
// Client.Store(), passed as host.WithStore and host.WithTrash(store.Trash()):
// the trash buckets an adapter records in a receipt are the ones apply
// restores from that store, so an adapter on another store makes every
// replaced artifact unrecoverable.
func WithHosts(hosts ...host.Host) Option {
	return func(c *config) {
		c.hosts = hosts
		c.hostsSet = true
	}
}

// Open resolves the facade: home discovery (or WithHome), the machine store
// (or WithStore / DefaultRoot), secrets (WithSecrets, else
// <home>/state/secrets.json) and the logger. It writes nothing and takes no
// locks; creating home or store layout is an explicit Ensure decision by the
// caller.
func Open(ctx context.Context, opts ...Option) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, &OpenError{Option: OptionCtx, Cause: err}
	}

	cfg := config{}

	for _, opt := range opts {
		opt(&cfg)
	}

	h, err := resolveHome(cfg)
	if err != nil {
		return nil, err
	}

	st, err := resolveStore(cfg)
	if err != nil {
		return nil, err
	}

	secrets, err := resolveSecrets(cfg, h)
	if err != nil {
		return nil, err
	}

	logger := cfg.logger
	if !cfg.loggerSet {
		logger = embedlog.NewLogger(false, false)
	}

	hosts, err := resolveHosts(cfg)
	if err != nil {
		return nil, err
	}

	now := cfg.now
	if now == nil {
		now = time.Now
	}

	client := &Client{home: h, store: st, secrets: secrets, logger: logger, configured: hosts, now: now}
	client.adapters = client.buildAdapters()

	return client, nil
}

// resolveHome applies WithHome or discovery.
func resolveHome(cfg config) (*home.Home, error) {
	if !cfg.homeSet {
		h, err := home.Discover()
		if err != nil {
			return nil, &OpenError{Option: OptionHome, Cause: err}
		}

		return h, nil
	}

	if strings.TrimSpace(cfg.homePath) == "" {
		return nil, &OpenError{Option: OptionHome, Value: cfg.homePath, Cause: errors.New("empty home path")}
	}

	h, err := home.New(cfg.homePath)
	if err != nil {
		return nil, &OpenError{Option: OptionHome, Value: cfg.homePath, Cause: err}
	}

	return h, nil
}

// resolveStore applies WithStore or the default store root.
func resolveStore(cfg config) (*store.Store, error) {
	root := ""

	if cfg.storeSet {
		if strings.TrimSpace(cfg.storeRoot) == "" {
			return nil, &OpenError{Option: OptionStore, Value: cfg.storeRoot, Cause: errors.New("empty store root")}
		}

		root = cfg.storeRoot
	}

	st, err := store.Open(root, storeOpts(cfg)...)
	if err != nil {
		return nil, &OpenError{Option: OptionStore, Value: root, Cause: err}
	}

	return st, nil
}

// storeOpts renders the settings the store itself owns. Only options a
// caller actually set are passed, so an unset one keeps the store's own
// default rather than a facade's copy of it.
func storeOpts(cfg config) []store.Option {
	var opts []store.Option

	if cfg.trashRetentionSet {
		opts = append(opts, store.WithTrashRetention(cfg.trashRetention))
	}

	return opts
}

// resolveSecrets applies WithSecrets or loads <home>/state/secrets.json.
func resolveSecrets(cfg config, h *home.Home) (SecretsStore, error) {
	if cfg.secretsSet {
		if cfg.secrets == nil {
			return nil, &OpenError{Option: OptionSecrets, Cause: errors.New("nil secrets store")}
		}

		return cfg.secrets, nil
	}

	path := h.SecretsPath()

	secrets, err := secret.Load(path)
	if err != nil {
		return nil, &OpenError{Option: OptionSecrets, Value: path, Cause: err}
	}

	return secrets, nil
}

// resolveHosts applies WithHosts; the adapters stay owned by the caller.
func resolveHosts(cfg config) ([]host.Host, error) {
	if !cfg.hostsSet {
		return nil, nil
	}

	for _, adapter := range cfg.hosts {
		if adapter == nil {
			return nil, &OpenError{Option: OptionHosts, Cause: errors.New("nil host adapter")}
		}
	}

	return slices.Clone(cfg.hosts), nil
}

// Close releases facade resources. In Ф0 nothing is held, so it is idempotent
// and always returns nil; resolved paths stay readable.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true

	return nil
}

// Home returns the resolved home handle; ownership stays with the client.
func (c *Client) Home() *home.Home {
	return c.home
}

// Store returns the resolved machine store handle; ownership stays with the
// client.
func (c *Client) Store() *store.Store {
	return c.store
}

// Secrets returns the resolved secrets store; ownership stays with the client.
func (c *Client) Secrets() SecretsStore {
	return c.secrets
}

// Hosts returns the host adapters this client delivers to: the list a caller
// injected with WithHosts, else the ten registered adapters built over this
// client's store, trash, ownership and secrets. Ownership stays with the client.
func (c *Client) Hosts() []host.Host {
	return slices.Clone(c.adapters)
}

// Logger returns the logger this client was opened with; an operation reports
// progress through it when the caller passed no event channel.
func (c *Client) Logger() embedlog.Logger {
	return c.logger
}

// OpenError.Option values: which Open step failed.
const (
	OptionHome    = "home"
	OptionStore   = "store"
	OptionSecrets = "secrets"
	OptionHosts   = "hosts"
	OptionCtx     = "ctx"
)

// OpenError reports a failed Open step.
type OpenError struct {
	Option string // OptionHome | OptionStore | OptionSecrets | OptionHosts | OptionCtx
	Value  string
	Cause  error
}

// Error implements error.
func (e *OpenError) Error() string {
	if e.Value != "" {
		return fmt.Sprintf("open verger: %s %q: %v", e.Option, e.Value, e.Cause)
	}

	return fmt.Sprintf("open verger: %s: %v", e.Option, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *OpenError) Unwrap() error {
	return e.Cause
}
