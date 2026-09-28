package host

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
)

// Cursor surface names, verified live against cursor-agent
// 2026.06.15-18-00-12-6f5a2cf (docs/reviews/cursor-grammar.probe.log): the
// agent reads <home>/.cursor/{skills,agents,mcp.json,hooks.json}.
const (
	wordCursorAgent = "cursor-agent"
	cursorDirName   = ".cursor"
	cursorSkillsDir = "skills"
	cursorAgentsDir = "agents"
	// cursorCommandsDir is the user slash-command directory: "~/.cursor/commands/*.md"
	// is the surface the host's own migrate-to-skills skill documents (User row).
	cursorCommandsDir = "commands"
	cursorMCPDoc      = "mcp.json"
	// cursorMCPListArgs is the host's own MCP listing: the CLI prints one
	// `<identifier>: <status>` line per configured server and has no JSON mode.
	cursorMCPListArgs = "list"
	// cursorHooksBlocked is the delivery note of the hook component: cursor
	// hooks are flat camelCase records (afterFileEdit, afterMCPExecution) in
	// hooks.json, a dialect the renderers do not carry yet. Writing the
	// claude-shaped document there would be invented, not delivered.
	cursorHooksBlocked = "cursor hooks are flat records with camelCase events in hooks.json, a dialect verger does not render yet"
	// cursorApprovalNote is the gate the host keeps for a written MCP server.
	cursorApprovalNote = "cursor loads an MCP server only after `cursor-agent mcp enable <name>` (or an approval prompt); a written server shows as \"needs approval\" until then"
)

// cursor is the Cursor agent adapter. It is loose-only on purpose: cursor-agent
// is a real CLI (the Ф1 design assumed a GUI-only host), but it exposes no
// subcommand that registers a package with the host — only mcp, worker, models
// and chat verbs — and the file-level plugin form the host documents
// (`<home>/.cursor/plugins/local/<name>`) is not something verger delivers yet.
// A GUI-side marketplace exists, so this is a gap in verger and in the CLI's
// scriptable surface, not a claim that the host cannot have plugins.
type cursor struct {
	base *Base
}

// NewCursor builds the Cursor adapter; the adapter is immutable after
// construction and safe for concurrent use.
func NewCursor(opts ...Option) Host {
	return &cursor{base: newBase(opts)}
}

// ID implements Host.
func (h *cursor) ID() ID {
	return Cursor
}

// Oracle implements Host.
func (h *cursor) Oracle() Oracle {
	return &cursorOracle{base: h.base}
}

// Detect reports whether the Cursor agent home exists or the `cursor-agent`
// binary resolves.
func (h *cursor) Detect(home string) bool {
	if isDir(cursorConfigDir(h.base.effectiveHome(home))) {
		return true
	}

	_, err := h.base.resolve(wordCursorAgent)

	return err == nil
}

// Deliver implements Host. The host has no installer, so native and synth are
// refused with the reason instead of pretending to deliver; loose is the only
// stratum.
func (h *cursor) Deliver(ctx context.Context, home string, d Delivery) (Result, error) {
	return deliverByStrategy(ctx, h.base, Cursor, home, d, h.deliverInstall, h.deliverLoose)
}

// deliverInstall refuses the CLI-driven strata: cursor-agent 2026.06.15 exposes
// mcp/worker/models/chat subcommands only, so verger has no scripted way to
// register a package; the host's documented file-level form
// (`<home>/.cursor/plugins/local/<name>`) is not implemented in this adapter yet.
func (h *cursor) deliverInstall(_ context.Context, _ string, _ Delivery, synth bool) (Result, error) {
	strategy := "native"
	if synth {
		strategy = "synth"
	}

	return Result{}, &NotSupportedError{
		Host:      Cursor,
		Operation: strategy + " delivery (no cursor-agent subcommand registers a package, and the host's file-level plugins/local form is not delivered by verger yet)",
	}
}

// cursorSpec is the loose surface of the Cursor adapter: skills, agents and
// slash commands below the agent home, MCP servers in its mcp.json. Its hook
// document is a dialect verger does not render yet, so hooks are skipped with a
// note rather than written somewhere invented; a rule component keeps the D23
// skill wrapper, which the delivery says out loud.
func cursorSpec(userHome string) looseSpec {
	dir := cursorConfigDir(userHome)

	return looseSpec{
		host:         Cursor,
		binary:       wordCursorAgent,
		home:         userHome,
		skillsDir:    filepath.Join(dir, cursorSkillsDir),
		agentsDir:    filepath.Join(dir, cursorAgentsDir),
		commandsDir:  filepath.Join(dir, cursorCommandsDir),
		settingsPath: filepath.Join(dir, "hooks.json"),
		hooksBlocked: cursorHooksBlocked,
		mcpConfig: &mcpConfigSpec{
			path:   filepath.Join(dir, cursorMCPDoc),
			format: manifest.FormatClaude,
			edit:   render.EditJSONC,
		},
	}
}

// deliverLoose plans and executes the host-native user files.
func (h *cursor) deliverLoose(ctx context.Context, home string, d Delivery) (Result, error) {
	userHome := h.base.effectiveHome(home)
	spec := cursorSpec(userHome)

	plan, err := planLoose(ctx, h.base, spec, d)
	if err != nil {
		return Result{}, err
	}

	if d.DryRun {
		return plan.result(d.Strategy, true), nil
	}

	err = h.base.executeLoose(ctx, spec, d.Package, plan)
	result := plan.result(d.Strategy, false)

	// A written server is not a loaded server: the host keeps its own approval
	// list, so the delivery says so instead of leaving the user guessing.
	if len(d.Package.MCP) > 0 && !d.DryRun {
		result.Notes = append(result.Notes, cursorApprovalNote)
	}

	// Cursor rules (.cursor/rules/*.mdc) are a project-scoped surface with no
	// user-level document this adapter verified, so a rule travels as the D23
	// skill wrapper instead of a guessed rule file.
	if slices.ContainsFunc(d.Package.Components, func(component manifest.Component) bool {
		return component.Kind == manifest.KindRule
	}) {
		result.Notes = append(result.Notes, "a rule is delivered as a skill wrapper (rule-<name> below "+
			filepath.Join(cursorDirName, cursorSkillsDir)+"); cursor's own rules surface is project-scoped and has no user document this adapter verified")
	}

	return result, err
}

// Uninstall implements Host: cursor records no host-install op (there is no
// installer), so the file and tree ops of the receipt are all there is.
func (h *cursor) Uninstall(ctx context.Context, _ string, r receipt.Receipt) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	notes, err := hostInverses(ctx, r, func(context.Context, receipt.Op) (string, error) {
		return "", &NotSupportedError{Host: Cursor, Operation: "a host-install inverse (cursor has no installer)"}
	})
	if err != nil {
		return Result{}, err
	}

	return Result{Strategy: Strategy(r.Strategy), Notes: notes}, nil
}

// cursorOracle is the Cursor agent oracle: its own MCP listing, never an LLM.
type cursorOracle struct {
	base *Base
}

// List implements Oracle: `cursor-agent mcp list` prints one
// `<identifier>: <status>` line per configured server, so the list is the
// host's own view of what it has. An unparsable line is skipped, never guessed
// into a server.
func (o *cursorOracle) List(ctx context.Context) ([]Installed, error) {
	out, err := o.base.run(ctx, wordCursorAgent, []string{"mcp", cursorMCPListArgs})
	if err != nil {
		return nil, err
	}

	return parseCursorMCP(out), nil
}

// Validate implements Oracle: cursor-agent has no validation command for a
// package directory (only `mcp list-tools` for one server), so validation is
// not supported by this host and no CLI call is made.
func (o *cursorOracle) Validate(context.Context, string) ([]string, error) {
	return nil, ErrNotSupported
}

// parseCursorMCP reads the `cursor-agent mcp list` lines. The status is free
// text, and the only administrative state the host reports is `disabled`:
// a configured server answers "not loaded (needs approval)" whether or not it
// has been approved, because this listing never starts a server (live-observed
// for 2026.06.15), so only `disabled` makes an entry not enabled. Two shapes are
// prose rather than a server and are skipped: an identifier carrying whitespace,
// and the host's informational line, which is worded "... configured ..."
// (live-captured: "No MCP servers configured (expected in .cursor/mcp.json or
// ~/.cursor/mcp.json)").
func parseCursorMCP(out []byte) []Installed {
	var listed []Installed

	for line := range strings.SplitSeq(string(out), "\n") {
		name, status, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || strings.TrimSpace(name) == "" || strings.ContainsAny(strings.TrimSpace(name), " \t") {
			continue
		}

		state := strings.ToLower(strings.TrimSpace(status))

		if strings.Contains(state, "configured") {
			continue
		}

		listed = append(listed, Installed{
			Name:    strings.TrimSpace(name),
			Enabled: !strings.Contains(state, "disabled"),
		})
	}

	return listed
}

// cursorConfigDir resolves the Cursor agent home: <home>/.cursor.
func cursorConfigDir(home string) string {
	return filepath.Join(home, cursorDirName)
}
