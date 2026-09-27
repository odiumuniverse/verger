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

	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Client is an open facade over one resolved home and machine store.
type Client struct {
	home    *home.Home
	store   *store.Store
	secrets *secret.Store
	logger  embedlog.Logger
	hosts   []host.Host

	mu     sync.Mutex
	closed bool
}

// config carries the option values of one Open call.
type config struct {
	homePath   string
	homeSet    bool
	storeRoot  string
	storeSet   bool
	secrets    *secret.Store
	secretsSet bool
	logger     embedlog.Logger
	loggerSet  bool
	hosts      []host.Host
	hostsSet   bool
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
func WithSecrets(s *secret.Store) Option {
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

	return &Client{home: h, store: st, secrets: secrets, logger: logger, hosts: hosts}, nil
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

	st, err := store.Open(root)
	if err != nil {
		return nil, &OpenError{Option: OptionStore, Value: root, Cause: err}
	}

	return st, nil
}

// resolveSecrets applies WithSecrets or loads <home>/state/secrets.json.
func resolveSecrets(cfg config, h *home.Home) (*secret.Store, error) {
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
func (c *Client) Secrets() *secret.Store {
	return c.secrets
}

// Hosts returns the configured host adapters; ownership stays with the client.
func (c *Client) Hosts() []host.Host {
	return slices.Clone(c.hosts)
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
