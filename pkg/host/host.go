// Package host delivers packages to one agent host under an explicit strategy:
// native (the host installer), synth (a chimera directory from the store) and
// loose (host-native user files). Adapters are immutable after construction and
// safe for concurrent use; the native→synth→loose→silenced ladder itself is
// pkg/plan's decision, not the adapter's.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/vmkteam/embedlog"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/store"
)

// ID identifies one agent host.
type ID string

// Host ids of Ф1 (the nine Ф1 ids plus omp, whose adapter arrives in Ф2;
// adapters are registered one task at a time).
const (
	Claude   ID = "claude"
	Codex    ID = "codex"
	Gemini   ID = "gemini"
	Agy      ID = "agy"
	Cursor   ID = "cursor"
	OpenCode ID = "opencode"
	Kilo     ID = "kilo"
	Pi       ID = "pi"
	DSH      ID = "dsh"
	Omp      ID = "omp"
)

// Strategy is how a package is delivered into a host.
type Strategy = lock.Strategy

// Strategy aliases shared with pkg/lock.
const (
	Native   = lock.StrategyNative
	Synth    = lock.StrategySynth
	Loose    = lock.StrategyLoose
	Silenced = lock.StrategySilenced
)

// Package is one package resolved for delivery to one host.
type Package struct {
	ID         string
	Version    string
	Format     manifest.Format
	Root       string // fetched payload root (or synth root for synth)
	SynthDir   string // set when StrategySynth
	Components []manifest.Component
	MCP        []manifest.MCPServer
	Hooks      []manifest.Hook // consent-approved subset only
	// Marketplace is the package's source ref ("<name>@<owner>", a registry
	// ref or a marketplace URL). The policy gate checks it for every strategy,
	// not only native: an empty ref is blocked by an allowlist policy
	// (strictKnownMarketplaces) and passes a blocklist.
	Marketplace string
	Scope       string // user|project
	ProjectRoot string // project scope root
	DataDir     string // ${PLUGIN_DATA} target (store/data/<pkg>/<host>)
}

// Delivery is one explicit-strategy delivery request.
type Delivery struct {
	Package    Package
	Strategy   Strategy
	AllowHooks bool // false for adopt and for pending consent
	DryRun     bool
	// Project is the trusted project root for a project-scope delivery, and
	// is ignored at user scope. Empty means "no project", which leaves a
	// project-scope delivery unsupported rather than guessing a root.
	Project string
	// Note is why this delivery was silenced, when it was. The ladder writes it
	// from the adapter's own words, so the reason reaches the user unchanged
	// rather than being re-derived here from a decision made elsewhere.
	Note string
}

// Result reports what one delivery or uninstall did.
type Result struct {
	Strategy  Strategy
	Artifacts []receipt.Artifact
	RMA       []receipt.Op
	Notes     []string
	Observed  OracleResult // last oracle observation
}

// OracleResult is what the host itself reported.
type OracleResult struct {
	Listed   []Installed
	Verified bool
}

// Installed is one plugin the host reports as installed.
type Installed struct {
	Name        string
	Marketplace string
	Version     string
	Path        string // install dir (Claude `installPath`)
	// Source is the install source the host reports for an entry that has no
	// marketplace concept: the specifier OpenCode's `plugin list` prints in
	// its SOURCE column.
	Source string
	Scope  string // host install scope (user|project|local), when reported
	// Enabled is false only when the host reports the plugin disabled (D27):
	// hosts that list no state list enabled plugins.
	Enabled bool
}

// Oracle talks to the host's own list/validate surface, never to an LLM.
type Oracle interface {
	// List returns what the host itself reports as installed.
	List(ctx context.Context) ([]Installed, error)
	// Validate asks the host to validate a synth directory.
	Validate(ctx context.Context, dir string) ([]string, error)
}

// Host delivers packages to one agent host.
type Host interface {
	ID() ID
	// Detect reports whether the host is present (binary or config home).
	Detect(home string) bool
	Oracle() Oracle
	// Deliver runs one explicit-strategy delivery. On an error after steps
	// already ran, the Result carries the RMA execution produced — every
	// planned op, with the trash buckets of what was replaced so far (NF-5) —
	// and callers roll back from it. An error before any step leaves the
	// Result empty; callers then roll back from the dry-run Result of the same
	// delivery, whose RMA covers every step the real run may have taken. An
	// OpHostInstall marked Existed names a host resource that existed before
	// this delivery: its inverse must not remove it on rollback.
	Deliver(ctx context.Context, home string, d Delivery) (Result, error)
	// Uninstall removes host-native traces recorded in a receipt RMA
	// (OpHostInstall); file/tree RMA is executed by pkg/apply.
	Uninstall(ctx context.Context, home string, r receipt.Receipt) (Result, error)
}

// Delivery note and refusal texts shared by the adapters.
const (
	noteDryRun               = "dry-run"
	unsupportedNoName        = "install without a package name"
	unsupportedNoMarketplace = "native install without a marketplace"
)

// Delivery step names shared by the adapters.
const (
	stepPlan      = "plan"
	stepInstall   = "install"
	stepVerify    = "verify"
	stepPolicy    = "policy"
	stepUninstall = "uninstall"
)

// isFile reports whether path is an existing regular file.
func isFile(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular()
}

// warningLines returns the non-empty trimmed lines of host output.
func warningLines(out []byte) []string {
	var warnings []string

	for line := range strings.SplitSeq(string(out), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			warnings = append(warnings, trimmed)
		}
	}

	return warnings
}

// isUnknownSubcommand reports whether the CLI rejected a subcommand itself
// (an unverified host grammar, OQ-T1.7.1/OQ-T1.8.1).
func isUnknownSubcommand(err error) bool {
	exit, ok := errors.AsType[*hostcli.ExitError](err)
	if !ok {
		return false
	}

	text := strings.ToLower(exit.Stderr)

	for _, marker := range []string{"unknown command", "unknown subcommand", "unrecognized subcommand", "no such command"} {
		if strings.Contains(text, marker) {
			return true
		}
	}

	return false
}

// deliverByStrategy is the shared adapter dispatch: a strategy the adapter
// never delivers is refused first, then the source-ref policy gate runs before
// any stratum (a host-forbidden source is never delivered — not even as
// files), then the requested explicit strategy runs. The ladder itself is
// pkg/plan, never the adapter.
func deliverByStrategy(
	ctx context.Context, base *Base, id ID, home string, d Delivery,
	install func(context.Context, string, Delivery, bool) (Result, error),
	loose func(context.Context, string, Delivery) (Result, error),
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	var stratum func() (Result, error)

	switch d.Strategy {
	case Native:
		stratum = func() (Result, error) { return install(ctx, home, d, false) }
	case Synth:
		stratum = func() (Result, error) { return install(ctx, home, d, true) }
	case Loose:
		stratum = func() (Result, error) { return loose(ctx, home, d) }
	case Silenced:
		// Silenced is a decision the ladder made, not a failure. It becomes an
		// error only if a caller asks for a strategy it cannot have, which is
		// what the default below still is: a strategy nobody chose, naming a
		// host that cannot produce it, is a bug in the plan and says so.
		//
		// The reason travels in Notes, so the cell can be recorded as delivered-
		// to-nothing with the explanation attached, and the user is told what
		// would have to change rather than receiving an error from a stratum
		// that was never going to be asked to do the work.
		note := d.Note
		if note == "" {
			note = "the host has no surface for this package"
		}

		return Result{Strategy: Silenced, Notes: []string{"silenced: " + note}}, nil
	default:
		return Result{}, &UnsupportedStrategyError{Host: id, Strategy: d.Strategy}
	}

	if err := checkPolicy(id, base.effectiveHome(home), d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	return stratum()
}

// PathOwner reports which package owns an existing path (receipts).
type PathOwner interface {
	Owner(path string) (pkg string, ok bool)
}

// ArtifactDigests is an optional PathOwner extension: the digest the owning
// receipt recorded for an artifact path. With it, re-delivering an unchanged
// CLI-managed MCP server skips the host call; without it the server is
// removed and re-added (NF-2).
type ArtifactDigests interface {
	ArtifactDigest(path string) (digest.Hash, bool)
}

// DefaultOracleWait bounds one service-backed oracle call: the OpenCode
// plugin verbs talk to the host's background service, which the CLI starts
// itself and retries forever when the port belongs to another instance, so the
// wait turns a hang into a note. It is the doctor's worst case, and a test
// pins it.
const DefaultOracleWait = 15 * time.Second

// DefaultLockWait is how long a host write waits for another writer's lock
// before backing off: the same 30s a sibling tool uses on the shared omp lock,
// so neither side can starve the other and the next run retries.
const DefaultLockWait = 30 * time.Second

// Base holds the shared adapter dependencies; adapters embed it read-only.
type Base struct {
	home        string
	lockWait    time.Duration
	oracleWait  time.Duration
	defaultWait time.Duration
	runner      hostcli.Runner
	logger      embedlog.Logger
	secrets     *secret.Store
	store       *store.Store
	trash       *store.Trash
	ownership   PathOwner
	resolver    *hostcli.Resolver
}

// Option configures a Base.
type Option func(*Base)

// WithLockWait sets how long a host write waits for another writer's lock; the
// zero value keeps DefaultLockWait.
func WithLockWait(wait time.Duration) Option {
	return func(b *Base) { b.lockWait = wait }
}

// WithOracleWait sets how long a service-backed oracle call waits before it
// reports the note. It overrides whatever default the adapter carries, so the
// seam behaves the same on every host: a test injects a short wait instead of
// sitting through the real bound, and a caller with a budget of its own can
// impose one.
func WithOracleWait(wait time.Duration) Option {
	return func(b *Base) { b.oracleWait = wait }
}

// withDefaultOracleWait is the adapter-level half of the same seam: a host
// that genuinely needs longer than DefaultOracleWait declares that as its own
// default, and WithOracleWait still wins over it. Without this the two
// defaults have to be kept out of the shared accessor, and a host that took
// the other path silently stops honouring the option.
func withDefaultOracleWait(wait time.Duration) Option {
	return func(b *Base) { b.defaultWait = wait }
}

// WithHome sets the fallback user home used for config paths.
func WithHome(home string) Option {
	return func(b *Base) { b.home = home }
}

// WithRunner replaces the host CLI runner; nil keeps hostcli.ExecRunner.
func WithRunner(r hostcli.Runner) Option {
	return func(b *Base) {
		if r != nil {
			b.runner = r
		}
	}
}

// WithLogger sets the embedlog logger used for diagnostics.
func WithLogger(l embedlog.Logger) Option {
	return func(b *Base) { b.logger = l }
}

// WithSecrets injects the secrets store used to resolve `{secret:NAME}`.
func WithSecrets(s *secret.Store) Option {
	return func(b *Base) { b.secrets = s }
}

// WithStore injects the machine store (plugin data dirs).
func WithStore(st *store.Store) Option {
	return func(b *Base) { b.store = st }
}

// WithTrash injects the trash used for replaced artifacts.
func WithTrash(tr *store.Trash) Option {
	return func(b *Base) { b.trash = tr }
}

// WithOwnership injects the receipt-backed path owner.
func WithOwnership(o PathOwner) Option {
	return func(b *Base) { b.ownership = o }
}

// newBase applies the options.
func newBase(opts []Option) *Base {
	b := &Base{runner: hostcli.ExecRunner{}, resolver: hostcli.NewResolver()}

	for _, opt := range opts {
		opt(b)
	}

	if b.runner == nil {
		b.runner = hostcli.ExecRunner{}
	}

	if b.resolver == nil {
		b.resolver = hostcli.NewResolver()
	}

	return b
}

func (b *Base) effectiveOracleWait() time.Duration {
	if b.oracleWait > 0 {
		return b.oracleWait
	}

	if b.defaultWait > 0 {
		return b.defaultWait
	}

	return DefaultOracleWait
}

// effectiveLockWait is the lock wait of this adapter.
func (b *Base) effectiveLockWait() time.Duration {
	if b.lockWait > 0 {
		return b.lockWait
	}

	return DefaultLockWait
}

// effectiveHome returns the delivery home, falling back to Base.home.
func (b *Base) effectiveHome(home string) string {
	if home != "" {
		return home
	}

	return b.home
}

// resolve finds one host CLI.
func (b *Base) resolve(name string) (hostcli.Binary, error) {
	return b.resolver.Resolve(name)
}

// run resolves and executes one host CLI.
func (b *Base) run(ctx context.Context, name string, args []string) ([]byte, error) {
	bin, err := b.resolve(name)
	if err != nil {
		return nil, err
	}

	return bin.RunWith(ctx, b.runner, args, nil)
}

// runStreams resolves and executes one host CLI, returning stderr too: some
// host CLIs print their results there (Gemini CLI 0.61).
func (b *Base) runStreams(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
	bin, err := b.resolve(name)
	if err != nil {
		return nil, nil, err
	}

	return bin.RunStreamsWith(ctx, b.runner, args, nil)
}

// ErrNotSupported reports an operation this host or strategy cannot do.
var ErrNotSupported = errors.New("not supported by this host")

// hostInverses runs the host-install ops of one RMA in reverse order — the
// order their forwards ran — and collects the notes of the ones already
// satisfied. Every adapter's Uninstall is this loop plus its own uninstallOp.
func hostInverses(ctx context.Context, r receipt.Receipt, run func(context.Context, receipt.Op) (string, error)) ([]string, error) {
	var notes []string

	for _, op := range slices.Backward(r.RMA) {
		if op.Kind != receipt.OpHostInstall {
			continue
		}

		note, err := run(ctx, op)
		if err != nil {
			return nil, err
		}

		if note != "" {
			notes = append(notes, note)
		}
	}

	return notes, nil
}

// marketplaceRefcount is the §4.8 refcount of one marketplace: it is kept
// while the oracle lists a plugin from it, and kept when the oracle cannot tell
// — removing a marketplace another plugin needs is worse than leaving it. The
// listing is the host's own oracle, read-only.
func marketplaceRefcount(ctx context.Context, host ID, listed func(context.Context) ([]Installed, error), marketplace string) (string, bool) {
	entries, err := listed(ctx)
	if err != nil {
		return fmt.Sprintf("%s plugin list failed (%v); marketplace %s kept", host, err, marketplace), true
	}

	serving := 0

	for _, entry := range entries {
		if entry.Marketplace == marketplace {
			serving++
		}
	}

	if serving > 0 {
		return fmt.Sprintf("marketplace %s still serves %d installed plugin(s); kept", marketplace, serving), true
	}

	return "", false
}

// UnsupportedStrategyError reports a strategy the adapter never delivers.
type UnsupportedStrategyError struct {
	Host     ID
	Strategy Strategy
}

// Error implements error.
func (e *UnsupportedStrategyError) Error() string {
	return fmt.Sprintf("%s: strategy %q is not delivered by the adapter", e.Host, e.Strategy)
}

// NotSupportedError reports a delivery step the host or package cannot satisfy.
type NotSupportedError struct {
	Host      ID
	Operation string
	Cause     error
}

// Error implements error.
func (e *NotSupportedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s is not supported: %v", e.Host, e.Operation, e.Cause)
	}

	return fmt.Sprintf("%s: %s is not supported", e.Host, e.Operation)
}

// Unwrap returns the underlying cause.
func (e *NotSupportedError) Unwrap() error {
	return e.Cause
}

// CollisionError reports a target path that belongs to someone else.
type CollisionError struct {
	Path  string
	Owner string
}

// Error implements error.
func (e *CollisionError) Error() string {
	if e.Owner == "" {
		return e.Path + " already exists and is not owned by verger"
	}

	return e.Path + " is owned by " + e.Owner
}

// UnsupportedVariableError reports a host-specific variable verger cannot
// resolve for the target host; nothing is written.
type UnsupportedVariableError struct {
	Path     string
	Variable string
}

// Error implements error.
func (e *UnsupportedVariableError) Error() string {
	return fmt.Sprintf("unsupported plugin variable %q in %s", e.Variable, e.Path)
}

// PolicyError reports a host managed policy forbidding a marketplace source;
// the source is never registered by any strategy.
type PolicyError struct {
	Host ID
	Rule string
	Ref  string
}

// Error implements error.
func (e *PolicyError) Error() string {
	if e.Ref == "" {
		return fmt.Sprintf("%s: a delivery without a source ref is blocked by policy %s", e.Host, e.Rule)
	}

	return fmt.Sprintf("%s: %q is blocked by policy %s", e.Host, e.Ref, e.Rule)
}

// MissingSecretsError reports `{secret:NAME}` references with no stored value;
// nothing is written and values never appear in the error.
type MissingSecretsError struct {
	Names []string
}

// Error implements error.
func (e *MissingSecretsError) Error() string {
	return fmt.Sprintf("missing secrets (values never leave the keychain): %v", e.Names)
}

// ReasonSecretInHook is the silenced reason of a SecretInHookError cell.
const ReasonSecretInHook = "secret-in-hook"

// SecretInHookError refuses a hook command carrying `{secret:NAME}` references
// (decision Q3): hook commands land in settings documents that are not 0600
// and may be committed in project scope, and their argv is visible in the
// process list. Only the names are reported, never a value; the cell is
// silenced with ReasonSecretInHook, and the error matches ErrNotSupported.
type SecretInHookError struct {
	Host  ID
	Event string
	Names []string
}

// Error implements error.
func (e *SecretInHookError) Error() string {
	return fmt.Sprintf("%s: the %s hook references secrets %v; secrets in hook commands are not delivered (%s)",
		e.Host, e.Event, e.Names, ReasonSecretInHook)
}

// Reason returns the silenced reason of the cell.
func (e *SecretInHookError) Reason() string {
	return ReasonSecretInHook
}

// Unwrap reports the refusal as an unsupported capability.
func (e *SecretInHookError) Unwrap() error {
	return ErrNotSupported
}

// OracleError reports host output the oracle cannot parse or a failed
// validation.
type OracleError struct {
	Host   string
	Output string
	Cause  error
}

// Error implements error.
func (e *OracleError) Error() string {
	return fmt.Sprintf("%s oracle: %v", e.Host, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *OracleError) Unwrap() error {
	return e.Cause
}

// DeliveryError reports a host-side delivery failure that is not a collision,
// a hands-off key or an unsupported capability.
type DeliveryError struct {
	Host    string
	Package string
	Step    string
	Cause   error
}

// Error implements error.
func (e *DeliveryError) Error() string {
	return fmt.Sprintf("%s: %s %s: %v", e.Host, e.Package, e.Step, e.Cause)
}

// Unwrap returns the underlying cause.
func (e *DeliveryError) Unwrap() error {
	return e.Cause
}
