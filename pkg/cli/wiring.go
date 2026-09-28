package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/pack"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/secret"
	"github.com/odiumuniverse/verger/pkg/source"
	"github.com/odiumuniverse/verger/pkg/spec"
	"github.com/odiumuniverse/verger/pkg/verger"
)

// scopePaths are the state paths of one scope.
type scopePaths struct {
	name           string // receipt.ScopeUser | receipt.ScopeProject
	root           string // verger home root or project root
	project        string // project root, project scope only
	specPath       string
	lockPath       string
	stateDir       string
	receiptsDir    string
	journalPath    string
	tombstonesPath string
}

// open opens the facade once per run.
func (a *app) open(ctx context.Context) (*verger.Client, error) {
	if a.client != nil {
		return a.client, nil
	}

	opts := slices.Clone(a.opts.openOpts)

	if a.homeFlag != "" {
		opts = append(opts, verger.WithHome(a.homeFlag))
	}

	if a.logger.Log() != nil {
		opts = append(opts, verger.WithLogger(a.logger))
	}

	client, err := verger.Open(ctx, opts...)
	if err != nil {
		return nil, err
	}

	a.client = client

	return client, nil
}

// paths resolves the state paths of the requested scope. Project scope lives
// in the repository: ./verger.toml, ./verger.lock and ./.verger/state (0700,
// OQ-T1.12.2).
func (a *app) paths(client *verger.Client) (scopePaths, error) {
	if a.projectFlag {
		wd, err := os.Getwd()
		if err != nil {
			return scopePaths{}, fmt.Errorf("resolve project root: %w", err)
		}

		state := filepath.Join(wd, ".verger", "state")

		return scopePaths{
			name:           receipt.ScopeProject,
			root:           wd,
			project:        wd,
			specPath:       filepath.Join(wd, "verger.toml"),
			lockPath:       filepath.Join(wd, "verger.lock"),
			stateDir:       state,
			receiptsDir:    filepath.Join(state, "receipts"),
			journalPath:    filepath.Join(state, "journal.jsonl"),
			tombstonesPath: filepath.Join(state, "tombstones.json"),
		}, nil
	}

	h := client.Home()

	return scopePaths{
		name:           receipt.ScopeUser,
		root:           h.Root(),
		specPath:       h.SpecPath(),
		lockPath:       h.LockPath(),
		stateDir:       h.StateDir(),
		receiptsDir:    h.ReceiptsDir(),
		journalPath:    h.JournalPath(),
		tombstonesPath: h.TombstonesPath(),
	}, nil
}

// ensure creates the home and store layout for a write command (rule 1).
func (a *app) ensure(client *verger.Client, paths scopePaths) error {
	if err := client.Home().Ensure(); err != nil {
		return err
	}

	if err := client.Store().Ensure(); err != nil {
		return err
	}

	if paths.name == receipt.ScopeProject {
		if err := fsutil.EnsureDir(paths.stateDir, 0o700); err != nil {
			return err
		}

		if err := excludeFromGit(paths.project); err != nil {
			return err
		}
	}

	return nil
}

// excludeFromGit adds the project state dir to .git/info/exclude when the
// project is a git repository (D11: generated files never become committable).
func excludeFromGit(project string) error {
	gitDir := filepath.Join(project, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return nil //nolint:nilerr // a non-git project has no exclude file
	}

	infoDir := filepath.Join(gitDir, "info")

	if err := fsutil.EnsureDir(infoDir, 0o700); err != nil {
		return err
	}

	path := filepath.Join(infoDir, "exclude")

	const entry = ".verger/"

	data, err := os.ReadFile(path) //nolint:gosec // G304: the project's own git dir
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read git exclude: %w", err)
	}

	if slices.Contains(strings.Split(string(data), "\n"), entry) {
		return nil
	}

	content := string(data)

	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	content += entry + "\n"

	return fsutil.WriteFileAtomic(path, []byte(content), 0o644)
}

// hosts resolves the host adapters: injected ones win, otherwise the Ф1
// adapters are constructed with the machine store and ownership.
func (a *app) hosts(client *verger.Client) []host.Host {
	if injected := client.Hosts(); len(injected) > 0 {
		return injected
	}

	options := []host.Option{
		host.WithStore(client.Store()),
		host.WithTrash(client.Store().Trash()),
		host.WithOwnership(receiptOwner{receipts: receipt.NewStore(client.Home().ReceiptsDir())}),
	}

	if a.logger.Log() != nil {
		options = append(options, host.WithLogger(a.logger))
	}

	if userHome, err := os.UserHomeDir(); err == nil {
		options = append(options, host.WithHome(userHome))
	}

	factories := []func(...host.Option) host.Host{host.NewClaude, host.NewCodex, host.NewGemini, host.NewOmp}

	out := make([]host.Host, 0, len(factories))

	for _, factory := range factories {
		out = append(out, factory(options...))
	}

	return out
}

// targets filters detected hosts by --hosts/--except. Unknown names are
// rejected by hostSet; a known name without a registered adapter is reported
// as unavailable (not as an unknown host).
func (a *app) targets(client *verger.Client, adapters []host.Host) ([]host.Host, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}

	only, err := hostSet(a.hostsFlag, "--hosts")
	if err != nil {
		return nil, err
	}

	except, err := hostSet(a.exceptFlag, "--except")
	if err != nil {
		return nil, err
	}

	var out []host.Host

	for _, adapter := range adapters {
		id := string(adapter.ID())

		if len(only) > 0 && !only[id] {
			continue
		}

		if except[id] || !adapter.Detect(userHome) {
			continue
		}

		out = append(out, adapter)
	}

	if len(out) == 0 && len(only) > 0 {
		return nil, fmt.Errorf("--hosts: no available adapter for %s (not registered or not detected)",
			strings.Join(slices.Sorted(maps.Keys(only)), ", "))
	}

	return out, nil
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

// hostIDKnown reports whether id is one of the nine Ф1 host ids.
func hostIDKnown(id string) bool {
	for _, known := range host.All() {
		if string(known) == id {
			return true
		}
	}

	return false
}

// receiptOwner resolves path ownership from the receipt store.
type receiptOwner struct {
	receipts *receipt.Store
}

// Owner implements host.PathOwner.
func (o receiptOwner) Owner(path string) (string, bool) {
	record, _, ok := o.artifact(path)

	return record.Package, ok
}

// ArtifactDigest implements host.ArtifactDigests: an unchanged MCP server is
// re-delivered without a host call (NF-2).
func (o receiptOwner) ArtifactDigest(path string) (digest.Hash, bool) {
	_, artifact, ok := o.artifact(path)

	return artifact.Digest, ok
}

// artifact finds the receipt and artifact recorded for one path.
func (o receiptOwner) artifact(path string) (receipt.Receipt, receipt.Artifact, bool) {
	list, err := o.receipts.List()
	if err != nil {
		return receipt.Receipt{}, receipt.Artifact{}, false
	}

	for _, record := range list {
		for _, artifact := range record.Artifacts {
			if artifact.Path == path {
				return record, artifact, true
			}
		}
	}

	return receipt.Receipt{}, receipt.Artifact{}, false
}

// applyDeps assembles the executor dependencies of one scope.
func (a *app) applyDeps(client *verger.Client, adapters []host.Host, paths scopePaths) (apply.Deps, error) {
	lockDoc, err := loadLock(paths.lockPath)
	if err != nil {
		return apply.Deps{}, err
	}

	hostMap := map[host.ID]host.Host{}

	for _, adapter := range adapters {
		hostMap[adapter.ID()] = adapter
	}

	receipts := receipt.NewStore(paths.receiptsDir)

	return apply.Deps{
		Home:       client.Home(),
		Store:      client.Store(),
		Receipts:   receipts,
		Journal:    receipt.OpenJournal(paths.journalPath),
		Tombstones: receipt.NewTombstoneStore(paths.tombstonesPath),
		Hosts:      hostMap,
		Owned:      receiptOwner{receipts: receipts},
		Lock:       lockDoc,
		LockPath:   paths.lockPath,
	}, nil
}

// loadLock reads a lock document; a missing file is an empty lock.
func loadLock(path string) (*lock.Lock, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the state file
	if errors.Is(err, fs.ErrNotExist) {
		return lock.New(), nil
	}

	if err != nil {
		return nil, fmt.Errorf("read lock %s: %w", path, err)
	}

	parsed, err := lock.Parse(data)
	if err != nil {
		return nil, err
	}

	return parsed, nil
}

// applyOptions renders the executor options of one run.
func (a *app) applyOptions(client *verger.Client) apply.Options {
	opts := apply.Options{
		DryRun: a.dryRun,
		Now:    a.now,
		Logger: a.logger,
	}

	if a.yes {
		// -y accepts defaults and never resolves a destructive conflict
		// (rule 14): conflicts are declined, not confirmed.
		opts.Confirm = yesConfirmer{}
	} else if a.interactive() {
		opts.Confirm = promptConfirmer{a: a}
	}

	return opts
}

// yesConfirmer accepts safe defaults and declines destructive questions.
type yesConfirmer struct{}

// Confirm implements apply.Confirmer.
func (yesConfirmer) Confirm(context.Context, apply.Question) (bool, error) {
	return false, nil
}

// promptConfirmer asks [y/N] on the TTY.
type promptConfirmer struct {
	a *app
}

// Confirm implements apply.Confirmer.
func (c promptConfirmer) Confirm(_ context.Context, q apply.Question) (bool, error) {
	return c.a.ask(q.Message, false)
}

// requireConfirmation gates one mutating run (§1.3): -y accepts the defaults,
// a dry run is a preview, and a detached stdin without -y stops after the plan
// was printed with the typed ErrConfirmationRequired. Callers invoke it after
// printing the plan and before any write.
func (a *app) requireConfirmation() error {
	if a.yes || a.dryRun || a.tty(os.Stdin) {
		return nil
	}

	return fmt.Errorf("%w: not a terminal; pass -y to accept the defaults", ErrConfirmationRequired)
}

// ask reads one [Y/n] or [y/N] answer; a detached stdin returns
// ErrConfirmationRequired.
func (a *app) ask(message string, defaultYes bool) (bool, error) {
	if !a.interactive() {
		return false, ErrConfirmationRequired
	}

	suffix := "[y/N]"

	if defaultYes {
		suffix = "[Y/n]"
	}

	if _, err := fmt.Fprintf(a.out, "%s %s ", message, suffix); err != nil {
		return false, err
	}

	reader := bufio.NewReader(a.in)

	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	case "":
		return defaultYes, nil
	default:
		return defaultYes, nil
	}
}

// hooksDecision resolves the D5 hooks question for one package: the --hooks
// flag overrides the spec default; an approved content hash skips the
// question; -y accepts the default; a detached stdin without -y refuses with
// ErrConfirmationRequired before anything is written.
func (a *app) hooksDecision(client *verger.Client, pkg host.Package, mode spec.HooksMode) (bool, error) {
	if len(pkg.Hooks) == 0 {
		return false, nil
	}

	switch mode {
	case spec.HooksNo:
		return false, nil
	case spec.HooksYes:
		if a.dryRun {
			return true, nil
		}

		return true, a.approveHooks(client, pkg)
	default:
		store, err := a.consentStore(client)
		if err != nil {
			return false, err
		}

		hash := consent.HookHash(pkg.ID, pkg.Version, pkg.Hooks)
		if store.HooksApproved(pkg.ID, pkg.Version, hash) {
			return true, nil
		}

		if a.dryRun {
			return true, nil // a preview: the default answer, nothing is written
		}

		if a.yes {
			return true, a.approveHooks(client, pkg)
		}

		allow, err := a.ask(fmt.Sprintf("Install hooks of %s %s on all agents?", pkg.ID, pkg.Version), true)
		if err != nil {
			return false, err
		}

		if allow {
			return true, a.approveHooks(client, pkg)
		}

		return false, nil
	}
}

// approveHooks records the current content hash of one package.
func (a *app) approveHooks(client *verger.Client, pkg host.Package) error {
	if len(pkg.Hooks) == 0 {
		return nil
	}

	store, err := a.consentStore(client)
	if err != nil {
		return err
	}

	hash := consent.HookHash(pkg.ID, pkg.Version, pkg.Hooks)

	if err := store.ApproveHooks(pkg.ID, pkg.Version, hash); err != nil {
		return err
	}

	return store.Save()
}

// consentStore loads the hooks consent store.
func (a *app) consentStore(client *verger.Client) (*consent.Store, error) {
	store := consent.NewStore(client.Home().ConsentPath())

	if err := store.Load(); err != nil {
		return nil, err
	}

	return store, nil
}

// trustStore loads the project trust store.
func (a *app) trustStore(client *verger.Client) (*consent.TrustStore, error) {
	store := consent.NewTrustStore(client.Home().TrustPath())

	if err := store.Load(); err != nil {
		return nil, err
	}

	return store, nil
}

// requireTrust refuses a project-scoped write when the project spec is absent
// or its hash is untrusted (D11).
func (a *app) requireTrust(client *verger.Client, paths scopePaths) error {
	if paths.name != receipt.ScopeProject {
		return nil
	}

	specDoc, err := spec.ParseFile(paths.specPath)
	if err != nil {
		return &TrustError{Path: paths.specPath, Cause: err}
	}

	store, err := a.trustStore(client)
	if err != nil {
		return err
	}

	trusted, err := store.Trusted(paths.project, consent.SpecHash(specDoc))
	if err != nil {
		return err
	}

	if !trusted {
		return &TrustError{Path: paths.specPath, Cause: errors.New("run `verger trust` in the project")}
	}

	return nil
}

// loadSpec reads one spec document; a missing file is an empty spec and
// present=false.
func loadSpec(path string) (*spec.Spec, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the spec file
	if errors.Is(err, fs.ErrNotExist) {
		return spec.New(), false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read spec %s: %w", path, err)
	}

	parsed, err := spec.Parse(data)
	if err != nil {
		return nil, false, err
	}

	return parsed, true, nil
}

// saveSpec writes one spec document, creating the parent directory.
func saveSpec(path string, doc *spec.Spec) error {
	if err := fsutil.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return doc.Save(path)
}

// addSpecPackage inserts a package entry when absent and returns whether the
// spec changed.
func addSpecPackage(doc *spec.Spec, id, version string) bool {
	for _, pkg := range doc.Packages {
		if pkg.ID == id {
			return false
		}
	}

	doc.Packages = append(doc.Packages, spec.Package{ID: id, Version: version})

	return true
}

// removeSpecPackage drops every entry with the id and reports the count.
func removeSpecPackage(doc *spec.Spec, id string) int {
	kept := make([]spec.Package, 0, len(doc.Packages))
	removed := 0

	for _, pkg := range doc.Packages {
		if matchesID(pkg.ID, id) {
			removed++

			continue
		}

		kept = append(kept, pkg)
	}

	doc.Packages = kept

	return removed
}

// matchesID reports whether a stored package id answers a CLI query: the exact
// id, a bare short name matching any owner, or the short name of a canonical
// `local:<name>` id.
func matchesID(stored, query string) bool {
	if stored == query {
		return true
	}

	if strings.Contains(query, "/") {
		return false
	}

	_, short := splitID(stored)
	if short == query {
		return true
	}

	return strings.TrimPrefix(short, "local:") == query
}

// setSpecPin stores or clears the version pin of one package.
func setSpecPin(doc *spec.Spec, id, version string) (bool, error) {
	for i := range doc.Packages {
		if doc.Packages[i].ID == id {
			doc.Packages[i].Version = version

			return true, nil
		}
	}

	return false, &UsageError{Cause: fmt.Errorf("package %q is not in the spec", id)}
}

// sourceName derives a stable source name from one ref.
func sourceName(ref source.Ref) string {
	switch {
	case ref.Repo != "":
		return ref.Repo
	case ref.Path != "":
		return filepath.Base(ref.Path)
	default:
		return filepath.Base(ref.Raw)
	}
}

// addSpecSource records one fetched ref as a spec source; an existing source
// with the same URL is kept as-is.
func addSpecSource(doc *spec.Spec, ref source.Ref) bool {
	raw := ref.Raw
	name := sourceName(ref)

	for _, src := range doc.Sources {
		if src.URL == raw {
			return false
		}

		if src.Name == name {
			suffix := digest.Bytes([]byte(raw)).String()
			name = name + "-" + suffix[len(suffix)-6:]
		}
	}

	doc.Sources = append(doc.Sources, spec.Source{Name: name, URL: raw})

	return true
}

// removeSpecSource drops every source with the name.
func removeSpecSource(doc *spec.Spec, name string) int {
	kept := make([]spec.Source, 0, len(doc.Sources))
	removed := 0

	for _, src := range doc.Sources {
		if src.Name == name {
			removed++

			continue
		}

		kept = append(kept, src)
	}

	doc.Sources = kept

	return removed
}

// buildHostPackage converts one fetched payload into one host delivery
// package.
func buildHostPackage(client *verger.Client, fetched *source.Fetched, id host.ID, version, scope, projectRoot string) (host.Package, error) {
	meta := fetched.Package

	dataDir, err := client.Store().PackageDataPath(idString(meta), string(id))
	if err != nil {
		return host.Package{}, err
	}

	pkg := host.Package{
		ID:          idString(meta),
		Version:     firstNonEmpty(version, meta.Version),
		Format:      meta.Format,
		Root:        fetched.Root,
		Components:  meta.Components,
		MCP:         meta.MCP,
		Hooks:       meta.Hooks,
		Scope:       scope,
		ProjectRoot: projectRoot,
		DataDir:     dataDir,
	}

	return pkg, nil
}

// idString renders the canonical id of one parsed manifest package.
func idString(meta *manifest.Package) string {
	if meta.ID != "" {
		return meta.ID
	}

	return meta.Name
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// pickStrategy is the Ф1 stopgap ladder (TODO T2.1, OQ-T1.12.1): native when
// the payload carries a marketplace ref and the host CLI resolves; synth for
// remote payloads pkg/pack can render; loose otherwise (local dev payloads are
// always loose, DESIGN §2.1).
func (a *app) pickStrategy(pkg host.Package, hostID host.ID, kind source.Kind) (host.Strategy, string) {
	if pkg.Marketplace != "" && nativeCLIResolves(hostID) {
		return host.Native, ""
	}

	if synthRenderable(pkg, kind) {
		return host.Synth, ""
	}

	return host.Loose, ""
}

// synthRenderable reports whether pkg/pack can render the package for a
// remote-sourced payload with an owner.
func synthRenderable(pkg host.Package, kind source.Kind) bool {
	if kind == source.KindLocal || kind == "" {
		return false
	}

	if owner, _ := splitID(pkg.ID); owner == "" {
		return false
	}

	input, err := packInput(pkg, pkg.Root)
	if err != nil {
		return false
	}

	_, err = pack.Render(input)

	return err == nil
}

// nativeCLIResolves reports whether the host's own CLI resolves on PATH.
func nativeCLIResolves(id host.ID) bool {
	name, ok := hostCLIName(id)
	if !ok {
		return false
	}

	_, err := hostcli.NewResolver().Resolve(name)

	return err == nil
}

// hostCLIName maps one host id to its CLI binary name.
func hostCLIName(id host.ID) (string, bool) {
	switch id {
	case host.Claude:
		return "claude", true
	case host.Codex:
		return "codex", true
	case host.Gemini:
		return "gemini", true
	case host.Agy:
		return "agy", true
	case host.Cursor:
		return "cursor", true
	case host.OpenCode:
		return "opencode", true
	case host.Kilo:
		return "kilo", true
	case host.Pi:
		return "pi", true
	case host.DSH:
		return "dsh", true
	case host.Omp:
		return "omp", true
	default:
		return "", false
	}
}

// packInput builds the chimera input of one host package.
func packInput(pkg host.Package, root string) (pack.Input, error) {
	owner, name := splitID(pkg.ID)
	if owner == "" || name == "" {
		return pack.Input{}, errors.New("the package id needs owner/name")
	}

	return pack.Input{
		ID: pkg.ID, Name: name, Owner: owner, Version: pkg.Version,
		Description: "", License: "", Format: pkg.Format,
		Root: root, Components: pkg.Components, MCP: pkg.MCP, Hooks: pkg.Hooks,
	}, nil
}

// splitID splits one owner/name id.
func splitID(id string) (string, string) {
	if index := strings.LastIndexByte(id, '/'); index >= 0 {
		return id[:index], id[index+1:]
	}

	return "", id
}

// prepareDelivery fills the strategy-specific fields of one delivery.
func (a *app) prepareDelivery(ctx context.Context, client *verger.Client, pkg host.Package, strategy host.Strategy) (host.Package, []string, error) {
	if strategy != host.Synth {
		return pkg, nil, nil
	}

	owner, name := splitID(pkg.ID)
	if owner == "" {
		return pkg, nil, errors.New("synth needs an owner/name package id")
	}

	input, err := packInput(pkg, pkg.Root)
	if err != nil {
		return pkg, nil, err
	}

	result, err := pack.Write(ctx, client.Store(), input)
	if err != nil {
		return pkg, nil, err
	}

	pkg.SynthDir = result.Dir
	pkg.Marketplace = name + "@" + owner

	return pkg, []string{"synth: " + result.Dir}, nil
}

// secretsStore returns the facade secrets store.
func (a *app) secretsStore(client *verger.Client) *secret.Store {
	return client.Secrets()
}

// printJSON writes one compact JSON document with a trailing newline.
func (a *app) printJSON(value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(a.out, string(data))

	return err
}

// cellDoc is the stable JSON shape of one applied cell.
type cellDoc struct {
	Package  string   `json:"package"`
	Host     string   `json:"host"`
	Scope    string   `json:"scope"`
	Status   string   `json:"status"`
	Version  string   `json:"version,omitempty"`
	Strategy string   `json:"strategy,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// reportDoc is the stable JSON shape of one executed plan.
type reportDoc struct {
	Cells []cellDoc `json:"cells"`
	Notes []string  `json:"notes,omitempty"`
	Home  string    `json:"home,omitempty"`
}

// reportDocFrom converts one apply report.
func reportDocFrom(report apply.Report) reportDoc {
	doc := reportDoc{Notes: report.Notes}

	for _, cell := range report.Cells {
		doc.Cells = append(doc.Cells, cellDoc{
			Package: cell.Package, Host: string(cell.Host), Scope: cell.Scope,
			Status: string(cell.Status), Version: cell.Version,
			Strategy: string(cell.Strategy), Kind: string(cell.Kind),
			Notes: cell.Notes,
		})
	}

	if doc.Cells == nil {
		doc.Cells = []cellDoc{}
	}

	return doc
}

// printReport renders one apply report as JSON or a table, then returns an
// *ApplyFailedError when a cell failed or was left in skew.
func (a *app) printReport(report apply.Report, homePath string) error {
	if err := a.renderReport(report, homePath); err != nil {
		return err
	}

	return failedCells(report)
}

// failedCells collects the failed and skewed cells of one report.
func failedCells(report apply.Report) error {
	var failed []string

	for _, cell := range report.Cells {
		if cell.Status == apply.StatusFailed || cell.Status == apply.StatusSkew {
			failed = append(failed, cell.Package+"@"+string(cell.Host))
		}
	}

	if len(failed) == 0 {
		return nil
	}

	return &ApplyFailedError{Cells: failed}
}

// renderReport writes one apply report as JSON or a table.
func (a *app) renderReport(report apply.Report, homePath string) error {
	if a.jsonOut {
		doc := reportDocFrom(report)
		doc.Home = homePath

		return a.printJSON(doc)
	}

	if len(report.Cells) == 0 {
		_, err := fmt.Fprintln(a.out, "nothing to do")

		return err
	}

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %-10s %s\n", "PACKAGE", "HOST", "SCOPE", "STATUS", "STRATEGY", "VERSION")

	for _, cell := range report.Cells {
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %-10s %s\n",
			cell.Package, cell.Host, cell.Scope, cell.Status, cell.Strategy, cell.Version)

		for _, note := range cell.Notes {
			_, _ = fmt.Fprintf(a.out, "  %s\n", note)
		}
	}

	for _, note := range report.Notes {
		_, _ = fmt.Fprintf(a.out, "note: %s\n", note)
	}

	return nil
}

// printPlan renders the per-host plan table before an apply (rule 2).
func (a *app) printPlan(cells []cellDoc) error {
	if a.jsonOut {
		return nil
	}

	if len(cells) == 0 {
		_, err := fmt.Fprintln(a.out, "plan: nothing to do")

		return err
	}

	_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %s\n", "PACKAGE", "HOST", "SCOPE", "STRATEGY", "VERSION")

	for _, cell := range cells {
		_, _ = fmt.Fprintf(a.out, "%-24s %-8s %-8s %-10s %s\n",
			cell.Package, cell.Host, cell.Scope, cell.Strategy, cell.Version)
	}

	return nil
}

// homeRoot returns the verger home root for display.
func homeRoot(client *verger.Client) string {
	return client.Home().Root()
}

// marshalJSON renders one compact, HTML-unescaped document.
func marshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(value); err != nil {
		return nil, err
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
