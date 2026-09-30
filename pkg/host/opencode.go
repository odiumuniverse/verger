package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/hostpath"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// OpenCode surface names and config values. The layout is the one OpenCode
// shares with beadle, byte for byte: `<config dir>/<xdg:opencode>` holds
// skills, agents, commands and the config document (docs/tasks/phase2-wave1.md,
// verified for agent frontmatter live on the fork, kilo 7.8.1).
const (
	wordOpenCode       = "opencode"
	flagVersion        = "--version"
	openCodeDirName    = "opencode"
	openCodeJSONC      = "opencode.jsonc"
	openCodeJSON       = "opencode.json"
	openCodeSkillsDir  = "skills"
	openCodeAgentsDir  = "agents"
	openCodeCommandDir = "commands"

	// openCodeMCPPrefixV2 and openCodeMCPPrefixV1 are the two MCP containers
	// the host reads; a delivery writes into the one the config already
	// declares, or into the v2 one on a config that declares neither.
	openCodeMCPPrefixV2 = "mcp.servers."
	openCodeMCPPrefixV1 = "mcp."

	openCodeMCPKey = "mcp"

	// openCodeHooksBlocked is the reason a hook component is skipped. opencode has
	// no session-start event to register on: the v2 event table carries null for
	// session-start, so a delivered module has nothing to fire on except a tool
	// call, and a tool call needs a model. The note names WHICH missing event, so
	// the block lifts the moment one exists (NIGHT-pR-8).
	openCodeHooksBlocked = "opencode has no session-start event to register on: the v2 event table carries null for session-start, so a delivered module fires only on a tool call, which needs a model (NIGHT-pR-8)"
)

// Host output markers captured live on opencode 2.0.18.
const (
	openCodeAlreadyConfigured = "is already configured"
	openCodeNotConfigured     = "is not configured"
	openCodeNoEntrypoint      = "has no server or TUI entrypoint"
)

// openCodeTableSep splits one `plugin list` row: the host pads the columns
// with two or more spaces (a specifier never carries one).
var openCodeTableSep = regexp.MustCompile(`\s{2,}`)

// opencodeDialect tags the plugin dialect of the installed host.
type opencodeDialect int

const (
	openCodeV1 opencodeDialect = iota
	openCodeV2
)

// String names the dialect the way the runtime's event table spells it, so
// the shim is rendered for the host that will load it.
func (d opencodeDialect) String() string {
	if d == openCodeV1 {
		return "v1"
	}

	return "v2"
}

// opencode is the OpenCode adapter. OpenCode 2.x hosts the plugin manager
// itself — `opencode plugin add|list|remove` (live-verified on 2.0.18, see
// docs/reviews/W1-A (opencode v1+v2, kilo)-*.log) — so packages that carry an
// OpenCode plugin are installed natively; everything else reaches the host as
// its own files. A 1.x host has no install verb (its plugin list is
// hand-written configuration), so only the loose strata are offered there.
//
// The host keeps a *running* background service and every `plugin` verb talks
// to it (`opencode service`); the CLI starts or reuses it itself, and a
// port already taken by another process makes it retry forever (live
// observation) — verger never manages that service, it only calls the CLI.
type opencode struct {
	base *Base
}

// NewOpenCode builds the OpenCode adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewOpenCode(opts ...Option) Host {
	return &opencode{base: newBase(opts)}
}

// ID implements Host.
func (h *opencode) ID() ID {
	return OpenCode
}

// Oracle implements Host.
func (h *opencode) Oracle() Oracle {
	return &openCodeOracle{base: h.base}
}

// Detect reports whether OpenCode is present: its config dir or its binary.
func (h *opencode) Detect(home string) bool {
	if isDir(openCodeConfigDir(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordOpenCode)

	return err == nil
}

// Deliver implements Host with explicit strategies; the ladder is pkg/plan.
func (h *opencode) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, OpenCode, home, d, h.deliverInstall, h.deliverLoose)
}

// hostEnv is the environment pkg/hostpath resolves a host root with: the
// process environment, this build's GOOS and the home the adapter delivers
// into. One helper so every adapter asks the resolver the same question.
func hostEnv(home string) hostpath.Env {
	return hostpath.Env{Home: home, GOOS: runtime.GOOS, Lookup: os.LookupEnv}
}

// configDir resolves the OpenCode config root the way the host does,
// live-probed on 2.0.18 (`opencode debug paths` in an isolated HOME, service
// started): OPENCODE_CONFIG_DIR replaces the root outright and wins over
// XDG_CONFIG_HOME, which wins over the home-relative `.config/opencode`. The
// value is used verbatim — a relative one included, exactly as the host reads
// it.
//
// The whole rule is hostpath's (the OpenCode root resolver in
// pkg/hostpath/hostpath.go), so this asks it instead of repeating it. An
// earlier version read OPENCODE_CONFIG_DIR here with its own os.Getenv and
// left hostpath to cover only XDG and the default. That left the adapter
// owning a second copy of a rule hostpath already models, and reading the
// process environment outside the caller-supplied Env — the contract
// hostpath exists to hold. The duplicated constant went with it.
func openCodeConfigDir(home string) string {
	roots, err := hostpath.Roots(hostpath.OpenCode, hostEnv(home))
	if err != nil {
		return filepath.Join(home, ".config", openCodeDirName)
	}

	return roots.ConfigRoot
}

// configFile resolves the config document: an existing opencode.jsonc wins
// over opencode.json, and a host with neither gets opencode.json (beadle's
// rule; 2.0.18 `plugin add` writes exactly this file).
func openCodeConfigFile(dir string) string {
	for _, name := range []string{openCodeJSONC, openCodeJSON} {
		if path := filepath.Join(dir, name); isFile(path) {
			return path
		}
	}

	return filepath.Join(dir, openCodeJSON)
}

// dialect resolves the plugin dialect: the resolved binary's major version
// decides when it answers `--version`; a host without a parseable binary falls
// back to the shape of its config document (beadle's rule), and a host with
// neither reads as v2, the current major.
func (h *opencode) dialect(ctx context.Context, home string) opencodeDialect {
	if major, ok := h.installedMajor(ctx); ok && major >= 2 {
		return openCodeV2
	} else if ok {
		return openCodeV1
	}

	if prefix, ok := openCodeDeclaredMCPPrefix(openCodeConfigFile(openCodeConfigDir(home))); ok {
		// The config declares a container; its shape is the host's own.
		if prefix == openCodeMCPPrefixV2 {
			return openCodeV2
		}

		return openCodeV1
	}

	return openCodeV2
}

// installedMajor probes the resolved binary for its major version. The probe
// goes through the adapter's runner, like every other host call: a test
// injects the answer, and a host that answers nothing reports no version, so
// the caller falls back to the config shape.
func (h *opencode) installedMajor(ctx context.Context) (int, bool) {
	if _, err := h.base.resolve(wordOpenCode); err != nil {
		return 0, false
	}

	out, err := h.base.run(ctx, wordOpenCode, []string{flagVersion})
	if err != nil {
		return 0, false
	}

	return openCodeMajor(string(out))
}

// mcpPrefix resolves the MCP container of the config document: the container
// the file already declares (so a delivery never grows a second one next to
// the servers the host reads), else the dialect's default.
func (h *opencode) mcpPrefix(home string, dialect opencodeDialect) string {
	if prefix, ok := openCodeDeclaredMCPPrefix(openCodeConfigFile(openCodeConfigDir(home))); ok {
		return prefix
	}

	if dialect == openCodeV2 {
		return openCodeMCPPrefixV2
	}

	return openCodeMCPPrefixV1
}

// openCodeMajor reads the major version out of an `opencode --version` line
// like `opencode v2.0.18`.
func openCodeMajor(line string) (int, bool) {
	for field := range strings.FieldsSeq(strings.TrimSpace(line)) {
		trimmed := strings.TrimLeft(field, "vV")

		major, rest, ok := strings.Cut(trimmed, ".")
		if !ok || rest == "" {
			continue
		}

		value, err := strconv.Atoi(major)
		if err != nil {
			continue
		}

		return value, true
	}

	return 0, false
}

// openCodeDeclaredMCPPrefix reads the MCP container the config document
// declares: `mcp.servers` is the v2 container, `mcp` the v1 one (beadle's
// rule). ok is false when the document declares neither, or does not parse.
func openCodeDeclaredMCPPrefix(path string) (string, bool) {
	file, err := readOptionalFile(path)
	if err != nil || len(file) == 0 {
		return "", false
	}

	mcp, found, err := configMember(file, openCodeMCPKey, false)
	if err != nil || !found {
		return "", false
	}

	container, ok := mcp.(map[string]any)
	if !ok {
		return "", false
	}

	if servers, has := container["servers"]; has {
		if _, ok := servers.(map[string]any); ok {
			return openCodeMCPPrefixV2, true
		}
	}

	return openCodeMCPPrefixV1, true
}

// openCodeSpec is the loose surface of the OpenCode adapter: skills, agents
// and commands below the config dir, MCP servers in the config document under
// the container the dialect declares.
//
//nolint:dupl // kilo and OpenCode share the OpenCode dialect; only the config roots differ
func openCodeSpec(userHome, prefix, dialect string) looseSpec {
	dir := openCodeConfigDir(userHome)

	return looseSpec{
		host:         OpenCode,
		binary:       wordOpenCode,
		home:         userHome,
		skillsDir:    filepath.Join(dir, openCodeSkillsDir),
		agentsDir:    filepath.Join(dir, openCodeAgentsDir),
		commandsDir:  filepath.Join(dir, openCodeCommandDir),
		hooksBlocked: openCodeHooksBlocked,
		// OpenCode has no declarative hook document: it runs hooks by
		// importing a module, so the shim is that module. The placement is
		// proven to LOAD against real 2.0.18, and that is all: no test here
		// shows a delivered hook EXECUTING, so the host stays blocked rather
		// than reporting hooks as delivered.
		runtimePlugin: &runtimePluginSpec{
			host:       string(OpenCode),
			dialect:    dialect,
			layout:     layoutDirectory,
			surface:    func(home, _ string) string { return openCodeConfigDir(home) },
			configFile: openCodeConfigFile,
		},
		renderAgent:   func(agent render.Agent, _ string) ([]byte, error) { return agent.OpenCodeMarkdown() },
		renderCommand: func(cmd render.Command) ([]byte, error) { return cmd.OpenCodeMarkdown() },
		mcpConfig: &mcpConfigSpec{
			path:   openCodeConfigFile(dir),
			format: manifest.FormatOpenCode,
			prefix: prefix,
			edit:   render.EditJSONC,
		},
	}
}

// deliverLoose plans and executes the host-native user files.
func (h *opencode) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)

	// One probe answers both questions: the dialect decides the plugin shape
	// and the MCP container, and a delivery that asks the host twice for the
	// same answer is a delivery that can be slow for no reason.
	dialect := h.dialect(ctx, userHome)

	return deliverSurface(ctx, h.base, openCodeSpec(userHome, h.mcpPrefix(userHome, dialect), dialect.String()), d)
}

// openCodeInstall is one native plugin install.
type openCodeInstall struct {
	spec    string // the specifier handed to `opencode plugin add`
	existed bool   // the host already configured the specifier
	config  string // the config document the host named in its answer
}

// rma is the recorded inverse: the host's own remove verb. An already
// configured plugin predates this delivery and is kept on rollback.
func (p openCodeInstall) rma() []receipt.Op {
	return []receipt.Op{{
		Kind:    receipt.OpHostInstall,
		Command: []string{wordPlugin, wordRemove, p.spec},
		Existed: p.existed,
	}}
}

// deliverInstall runs the native rung on a v2 host: register the package
// through the host's own plugin manager and verify it. Everything the host
// cannot take is refused with the reason, never guessed: synth (the rendered
// v2 plugin arrives with T2.3), the v1 dialect (no install verb), a ref that
// is not an npm or Git specifier, and a package the host itself reports as
// carrying no plugin entrypoint.
func (h *opencode) deliverInstall(ctx context.Context, home string, d Delivery, synth bool) (Result, error) {
	userHome := h.base.effectiveHome(home)

	if err := checkCommandSources(OpenCode, userHome, d.Package.Marketplace); err != nil {
		return Result{}, err
	}

	if synth {
		return Result{}, &NotSupportedError{
			Host:      OpenCode,
			Operation: "synth delivery (a rendered v2 plugin arrives with T2.3; until then a package reaches OpenCode as files)",
		}
	}

	// The ref is settled before the host is probed: a package the native rung
	// cannot take is refused without a single CLI call.
	spec, ok := openCodePluginSpec(d.Package.Marketplace)
	if !ok {
		return Result{}, &NotSupportedError{Host: OpenCode, Operation: openCodeRefReason(d.Package.Marketplace)}
	}

	if h.dialect(ctx, userHome) != openCodeV2 {
		return Result{}, &NotSupportedError{
			Host:      OpenCode,
			Operation: "native install on OpenCode 1.x (the 1.x plugin list is hand-written configuration, written with the runtime shim, T2.3)",
		}
	}

	plan := openCodeInstall{spec: spec, config: openCodeConfigFile(openCodeConfigDir(userHome))}

	if d.DryRun {
		// A dry run installs nothing, so its inverse may not claim the right to
		// remove an entry: the real run corrects this from the host's answer.
		plan.existed = true

		return Result{Strategy: d.Strategy, RMA: plan.rma(), Notes: []string{noteDryRun}}, nil
	}

	out, err := h.base.run(ctx, wordOpenCode, []string{wordPlugin, wordAdd, spec})
	if err != nil {
		if refusedAsNonPlugin(err) {
			return Result{}, &NotSupportedError{
				Host:      OpenCode,
				Operation: fmt.Sprintf("native install of %s: the package carries no OpenCode plugin entrypoint", d.Package.Marketplace),
				Cause:     err,
			}
		}

		return Result{}, &DeliveryError{Host: string(OpenCode), Package: d.Package.ID, Step: stepInstall, Cause: err}
	}

	plan.existed = strings.Contains(string(out), openCodeAlreadyConfigured)

	if named := openCodeAddedPath(string(out)); named != "" {
		plan.config = named
	}

	observed, notes, err := h.verify(ctx, d.Package, plan)

	return Result{Strategy: d.Strategy, RMA: plan.rma(), Observed: observed, Notes: notes}, err
}

// verify answers whether the delivery is really in the host. Two pieces of
// evidence are checked, in this order: the config document the host itself
// wrote (it names the specifier in its `plugins` list), and the oracle's live
// listing. The listing reflects the plugin registry of the *running* service,
// which loads plugins at start and does not pick up a config change until it
// next starts (live-observed on 2.0.18), so a lagging listing against a
// document that carries the specifier is a note for the operator, not a
// failure — while a document that does not carry it means the host took
// nothing, which is a *DeliveryError at the verify step.
func (h *opencode) verify(ctx context.Context, pkg Package, plan openCodeInstall) (OracleResult, []string, error) {
	listed, err := h.Oracle().List(ctx)
	if err != nil {
		return OracleResult{}, nil, &DeliveryError{Host: string(OpenCode), Package: pkg.ID, Step: stepVerify, Cause: err}
	}

	observed := OracleResult{Listed: listed}

	for _, entry := range listed {
		if entry.Source == plan.spec && entry.Enabled {
			observed.Verified = true

			return observed, nil, nil
		}
	}

	if !openCodeConfigHasPlugin(plan.config, plan.spec) {
		return observed, nil, &DeliveryError{
			Host:    string(OpenCode),
			Package: pkg.ID,
			Step:    stepVerify,
			Cause:   fmt.Errorf("the host does not carry %q in %s", plan.spec, plan.config),
		}
	}

	return observed, []string{fmt.Sprintf(
		"opencode configured %s in %s; its running background service lists the plugins it loaded at start, so restart OpenCode (or `opencode service restart`) to load it",
		plan.spec, plan.config)}, nil
}

// openCodeAddedPath reads the config document out of the host's own answer
// ("installed and added to <path>" / "is already configured in <path>").
func openCodeAddedPath(out string) string {
	for _, marker := range []string{"added to ", "configured in "} {
		if _, path, found := strings.Cut(out, marker); found {
			return strings.TrimSpace(path)
		}
	}

	return ""
}

// openCodeConfigHasPlugin reports whether the config document carries the
// specifier in its `plugins` list — the document the host itself reads and
// writes (JSONC, so comments do not stop the read). An unreadable or
// unparsable document is "not carried": the caller reports the miss.
func openCodeConfigHasPlugin(path, spec string) bool {
	file, err := readOptionalFile(path)
	if err != nil || len(file) == 0 {
		return false
	}

	plugins, found, err := configMember(file, "plugins", false)
	if err != nil || !found {
		return false
	}

	entries, ok := plugins.([]any)
	if !ok {
		return false
	}

	for _, entry := range entries {
		if name, ok := entry.(string); ok && name == spec {
			return true
		}
	}

	return false
}

// openCodeRefReason names why a source ref cannot be installed natively.
func openCodeRefReason(ref string) string {
	if ref == "" {
		return unsupportedNoMarketplace
	}

	return fmt.Sprintf("native install from %q (opencode plugin add takes an npm or Git package specifier)", ref)
}

// openCodePluginSpec translates a verger source ref into the specifier the
// host accepts, live-verified on 2.0.18: a bare npm name (optionally
// `@version`) or a Git specifier. The `npm:` ref prefix verger uses is not
// part of the host grammar and is stripped; every other shape is refused
// rather than guessed.
func openCodePluginSpec(ref string) (string, bool) {
	if ref == "" {
		return "", false
	}

	if name, ok := strings.CutPrefix(ref, "npm:"); ok {
		if name == "" || openCodeLocalPath(name) {
			return "", false
		}

		return name, true
	}

	switch {
	case strings.HasPrefix(ref, "git+"), strings.HasPrefix(ref, "git@"):
		return ref, true
	default:
		return "", false
	}
}

// openCodeLocalPath reports whether a value looks like a local path, which the
// host's own validation refuses.
func openCodeLocalPath(value string) bool {
	return strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") ||
		strings.HasPrefix(value, "../") || strings.HasPrefix(value, "file://")
}

// refusedAsNonPlugin reports whether the host refused a package because it
// carries no plugin entrypoint: the one install refusal the ladder answers
// with the loose rung instead of a failure.
func refusedAsNonPlugin(err error) bool {
	exit, ok := errors.AsType[*hostcli.ExitError](err)

	return ok && strings.Contains(exit.Stderr, openCodeNoEntrypoint)
}

// Uninstall runs the host-install RMA ops in reverse order; file/tree ops are
// executed by pkg/apply. The host answers `plugin remove` for a plugin it does
// not have with exit 0 and "is not configured", so an already-removed plugin
// is a note, not an error.
func (h *opencode) Uninstall(ctx context.Context, _ string, r receipt.Receipt) (Result, error) {
	notes, err := hostInverses(ctx, r, h.uninstallOp)

	return Result{Strategy: Strategy(r.Strategy), Notes: notes}, err
}

// uninstallOp runs one host-install inverse.
func (h *opencode) uninstallOp(ctx context.Context, op receipt.Op) (string, error) {
	if len(op.Command) == 0 {
		return "", fmt.Errorf("opencode: host-install op carries no argv (%+v)", op)
	}

	argv := op.Command

	if op.Existed {
		return strings.Join(argv, " ") + " existed before this delivery; kept", nil
	}

	out, err := h.base.run(ctx, wordOpenCode, argv)
	if err == nil {
		if strings.Contains(string(out), openCodeNotConfigured) {
			return argv[len(argv)-1] + " was not configured; nothing to remove", nil
		}

		return "", nil
	}

	if exit, refused := errors.AsType[*hostcli.ExitError](err); refused && strings.Contains(exit.Stderr, openCodeNotConfigured) {
		return argv[len(argv)-1] + " was not configured; nothing to remove", nil
	}

	return "", err
}

// openCodeOracle is the OpenCode CLI oracle: `plugin list`, never an LLM.
type openCodeOracle struct {
	base *Base
}

// The oracle wait is host.DefaultOracleWait, injectable through
// host.WithOracleWait so a test does not sit through it.
//
// List implements Oracle: `opencode plugin list` prints the `ID VERSION
// SOURCE` table of the plugins the host loaded, or the sentence "No plugins
// found" (both captured live on 2.0.18). The CLI offers no `--json` on this
// verb, so the table is parsed; a configured plugin the host could not
// resolve is listed with `-` in the id and version columns and counts as
// disabled. The call is bounded by that wait: the answer comes from
// the host's background service, and a service that never comes up must not
// hang the caller.
func (o *openCodeOracle) List(ctx context.Context) ([]Installed, error) {
	wait := o.base.effectiveOracleWait()

	// The caller's own deadline can be the shorter one — doctor gives every
	// host a shared budget — so what this call may wait is the smaller of the
	// two, and that is the number the note has to name. It is read before
	// the call, because afterwards the deadline has passed and the
	// remaining budget is zero whatever was really spent.
	spent := wait
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left > 0 && left < spent {
			spent = left
		}
	}

	bounded, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	out, err := o.base.run(bounded, wordOpenCode, []string{wordPlugin, wordList})
	if err != nil {
		if bounded.Err() != nil {
			return nil, o.serviceError(ctx, spent)
		}

		return nil, err
	}

	return parseOpenCodePlugins(string(out)), nil
}

// serviceLivenessWait bounds the liveness question. The verb does not touch
// the background service, so it either answers at once or the binary itself is
// wedged; there is nothing to wait for.
const serviceLivenessWait = 2 * time.Second

// serviceError says which of the two failures it is. A binary that answers
// while the listing does not means nothing is listening — a fact with a
// command attached — and calling that a timeout sends the reader off to wait
// longer for a service that was never running.
func (o *openCodeOracle) serviceError(ctx context.Context, spent time.Duration) error {
	// The liveness question is asked on a context of its own. The one that
	// arrived here has just spent its whole budget on the listing, so a
	// child of it is born expired and every probe would "fail" — which is
	// how a healthy binary gets reported as wedged.
	live, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceLivenessWait)
	defer cancel()

	if _, err := o.base.run(live, wordOpenCode, []string{"--version"}); err != nil {
		return &OracleError{
			Host: string(OpenCode),
			Cause: fmt.Errorf("%s did not answer within %s and the binary does not answer either, so the background service behind these verbs is wedged rather than down: start it with `opencode service start`",
				strings.Join([]string{wordOpenCode, wordPlugin, wordList}, " "), spent.Round(time.Millisecond)),
		}
	}

	return &OracleError{
		Host: string(OpenCode),
		Cause: fmt.Errorf("the %s service is not listening: %s did not answer within %s (start it with `opencode service start`; a port taken by another OpenCode instance makes the CLI retry forever)",
			wordOpenCode, strings.Join([]string{wordOpenCode, wordPlugin, wordList}, " "), spent.Round(time.Millisecond)),
	}
}

// Validate implements Oracle: opencode 2.0.18 validates a package only inside
// `plugin add` (`plugin list` is a listing, `plugin check` an update check), so
// validation is not supported by this host and no CLI call is made. The
// contract is pinned by TestOpenCodeOracleValidate: a caller must treat
// ErrNotSupported as "this host cannot validate", never as "valid".
func (o *openCodeOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// parseOpenCodePlugins reads the `opencode plugin list` table:
//
//	ID            VERSION  SOURCE
//	probe-plugin  becf70d  git+file:///tmp/ocprobe/pkg
//
// and the empty form ("No plugins found"). A row counts only when it is
// separated into at least three fields by two or more spaces, so a rewording
// of the header or a stray line cannot invent a plugin.
func parseOpenCodePlugins(out string) []Installed {
	var listed []Installed

	for line := range strings.SplitSeq(out, "\n") {
		fields := splitTableRow(line)

		if len(fields) < 3 {
			continue
		}

		if fields[0] == "ID" && fields[1] == "VERSION" {
			continue
		}

		listed = append(listed, Installed{
			Name:    fields[0],
			Version: fields[1],
			Source:  strings.Join(fields[2:], " "),
			Scope:   receipt.ScopeUser,
			Enabled: fields[0] != "-",
		})
	}

	return listed
}

// splitTableRow splits one padded table row on runs of whitespace.
func splitTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}

	return openCodeTableSep.Split(trimmed, -1)
}
