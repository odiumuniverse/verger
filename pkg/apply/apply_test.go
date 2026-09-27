package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/store"
)

// Fixture identities shared by the apply tests.
const (
	fxPkg     = "acme/tool"
	fxVersion = "1.0.0"
	fxNext    = "2.0.0"
	fxClaude  = host.Claude
	fxCodex   = host.Codex
	fxUser    = receipt.ScopeUser
)

// ---- fake host ---------------------------------------------------------------

// fakeFile is one artifact the fake host writes.
type fakeFile struct {
	Path string
	Data string
}

// tracker counts concurrent deliveries across fake hosts.
type tracker struct {
	mu  sync.Mutex
	cur int
	max int
}

func (t *tracker) enter() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cur++

	if t.cur > t.max {
		t.max = t.cur
	}
}

func (t *tracker) leave() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cur--
}

// fakeHost is a scripted host.Host: it writes configured files, trashes
// replaced ones, records calls and can fail, block or collide on demand.
type fakeHost struct {
	id    host.ID
	trash *store.Trash
	track *tracker

	mu        sync.Mutex
	targets   map[string][]fakeFile
	notes     map[string][]string
	verify    map[string]bool
	collide   map[string]int
	collideAt map[string]string
	gates     map[string]chan struct{}
	probes    map[string]string
	probeOK   map[string]bool

	calls       []string
	delivers    map[string]int
	uninstalls  int
	probeAtUn   map[string]bool
	inFlight    int
	maxInFlight int
	oracleList  []host.Installed
	oracleErr   error
	uninstErr   error
	deliverErr  error
	signal      chan struct{}
}

func newFake(id host.ID, tr *store.Trash) *fakeHost {
	return &fakeHost{
		id:        id,
		trash:     tr,
		targets:   map[string][]fakeFile{},
		notes:     map[string][]string{},
		verify:    map[string]bool{},
		collide:   map[string]int{},
		collideAt: map[string]string{},
		gates:     map[string]chan struct{}{},
		probes:    map[string]string{},
		probeOK:   map[string]bool{},
		delivers:  map[string]int{},
		probeAtUn: map[string]bool{},
	}
}

func (f *fakeHost) write(pkg string, files ...fakeFile) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.targets[pkg] = files

	return f
}

func (f *fakeHost) failVerify(pkg string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.verify[pkg] = true

	return f
}

func (f *fakeHost) collideOnce(pkg, path string, times int) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.collide[pkg] = times
	f.collideAt[pkg] = path

	return f
}

func (f *fakeHost) blockOn(pkg string, gate chan struct{}) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gates[pkg] = gate

	return f
}

func (f *fakeHost) probe(pkg, path string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.probes[pkg] = path

	return f
}

func (f *fakeHost) note(pkg, note string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.notes[pkg] = append(f.notes[pkg], note)

	return f
}

// ID implements host.Host.
func (f *fakeHost) ID() host.ID { return f.id }

// Detect implements host.Host.
func (f *fakeHost) Detect(string) bool { return true }

// Oracle implements host.Host.
func (f *fakeHost) Oracle() host.Oracle { return &fakeOracle{host: f} }

// deliverState is the knob snapshot of one Deliver call.
type deliverState struct {
	targets    []fakeFile
	notes      []string
	gate       chan struct{}
	collide    int
	collideAt  string
	probe      string
	verify     bool
	deliverErr error
}

// Deliver implements host.Host.
func (f *fakeHost) Deliver(ctx context.Context, _ string, d host.Delivery) (host.Result, error) {
	state := f.snapshot(d)

	f.beginDelivery(d)

	defer f.endDelivery(d)

	if err := f.waitGate(ctx, d, state.gate); err != nil {
		return host.Result{}, err
	}

	if state.deliverErr != nil {
		return host.Result{}, state.deliverErr
	}

	if state.collide > 0 {
		f.consumeCollision(d)

		return host.Result{}, &host.CollisionError{Path: state.collideAt}
	}

	f.probeNow(d, state.probe)

	result, err := f.plan(ctx, d, state.targets)
	if err != nil {
		return host.Result{}, err
	}

	result.Notes = state.notes

	return f.finishDelivery(d, state, result)
}

// snapshot records the call and copies the knobs.
func (f *fakeHost) snapshot(d host.Delivery) deliverState {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, "deliver "+string(d.Strategy)+" "+d.Package.ID+" dry="+boolText(d.DryRun))

	return deliverState{
		targets:    slices.Clone(f.targets[d.Package.ID]),
		notes:      slices.Clone(f.notes[d.Package.ID]),
		gate:       f.gates[d.Package.ID],
		collide:    f.collide[d.Package.ID],
		collideAt:  f.collideAt[d.Package.ID],
		probe:      f.probes[d.Package.ID],
		verify:     f.verify[d.Package.ID],
		deliverErr: f.deliverErr,
	}
}

// beginDelivery counts a real delivery and signals the test.
func (f *fakeHost) beginDelivery(d host.Delivery) {
	if d.DryRun {
		return
	}

	if f.signal != nil {
		select {
		case f.signal <- struct{}{}:
		default:
		}
	}

	f.mu.Lock()
	f.delivers[d.Package.ID]++
	f.inFlight++

	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}

	f.mu.Unlock()

	if f.track != nil {
		f.track.enter()
	}
}

// endDelivery releases the concurrency counters of one delivery.
func (f *fakeHost) endDelivery(d host.Delivery) {
	if d.DryRun {
		return
	}

	if f.track != nil {
		f.track.leave()
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

// waitGate blocks a real delivery until the gate opens or ctx ends.
func (f *fakeHost) waitGate(ctx context.Context, d host.Delivery, gate chan struct{}) error {
	if gate == nil || d.DryRun {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gate:
		return nil
	}
}

// consumeCollision spends one scripted collision.
func (f *fakeHost) consumeCollision(d host.Delivery) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.collide[d.Package.ID]--
}

// probeNow records whether the probe path exists at delivery time.
func (f *fakeHost) probeNow(d host.Delivery, path string) {
	if path == "" {
		return
	}

	_, err := os.Lstat(path)

	f.mu.Lock()
	f.probeOK[d.Package.ID] = err == nil
	f.mu.Unlock()
}

// finishDelivery applies the dry-run and verify knobs.
func (f *fakeHost) finishDelivery(d host.Delivery, state deliverState, result host.Result) (host.Result, error) {
	if d.DryRun {
		result.Notes = append(slices.Clone(result.Notes), "dry-run")

		return result, nil
	}

	if state.verify {
		return host.Result{}, &host.DeliveryError{
			Host: string(f.id), Package: d.Package.ID, Step: "verify",
			Cause: errors.New("the oracle does not list " + d.Package.ID),
		}
	}

	return result, nil
}

// plan renders (dry) or writes (real) the configured targets.
func (f *fakeHost) plan(ctx context.Context, d host.Delivery, targets []fakeFile) (host.Result, error) {
	result := host.Result{Strategy: d.Strategy, RMA: []receipt.Op{}}

	for _, target := range targets {
		data := []byte(target.Data)

		if d.DryRun {
			existed := fileExists(target.Path)
			sum := digest.Bytes(data) // the digest the delivery will write

			result.Artifacts = append(result.Artifacts, receipt.Artifact{
				Kind: "skill", Name: filepath.Base(target.Path), Path: target.Path, Digest: sum,
			})
			result.RMA = append(result.RMA, receipt.Op{
				Kind: receipt.OpWriteFile, Path: target.Path, Digest: sum, Mode: 0o600, Existed: existed,
			})

			continue
		}

		op := receipt.Op{Kind: receipt.OpWriteFile, Path: target.Path, Mode: 0o600}

		if fileExists(target.Path) {
			entry, err := f.trash.Put(ctx, target.Path, store.PutOptions{Package: d.Package.ID, Host: string(f.id)})
			if err != nil {
				return host.Result{}, err
			}

			op.Existed = true
			op.Backup = entry.ID
		}

		if err := fsutil.EnsureDir(filepath.Dir(target.Path), 0o700); err != nil {
			return host.Result{}, err
		}

		if err := fsutil.WriteFileAtomic(target.Path, data, 0o600); err != nil {
			return host.Result{}, err
		}

		op.Digest = digest.Bytes(data)
		result.Artifacts = append(result.Artifacts, receipt.Artifact{
			Kind: "skill", Name: filepath.Base(target.Path), Path: target.Path, Digest: op.Digest,
		})
		result.RMA = append(result.RMA, op)
	}

	return result, nil
}

// Uninstall implements host.Host.
func (f *fakeHost) Uninstall(_ context.Context, _ string, r receipt.Receipt) (host.Result, error) {
	f.mu.Lock()
	f.uninstalls++
	f.calls = append(f.calls, "uninstall "+r.Package)

	probe, ok := f.probes[r.Package]
	if ok {
		_, err := os.Lstat(probe) //nolint:gosec // G703: the probe path is a test-owned temp path
		f.probeAtUn[r.Package] = err == nil
	}

	err := f.uninstErr
	f.mu.Unlock()

	if err != nil {
		return host.Result{}, err
	}

	return host.Result{Strategy: host.Strategy(r.Strategy)}, nil
}

// fakeOracle is the fake host's oracle.
type fakeOracle struct {
	host *fakeHost
}

// List implements host.Oracle.
func (o *fakeOracle) List(context.Context) ([]host.Installed, error) {
	o.host.mu.Lock()
	defer o.host.mu.Unlock()

	o.host.calls = append(o.host.calls, "oracle")

	return slices.Clone(o.host.oracleList), o.host.oracleErr
}

// Validate implements host.Oracle.
func (o *fakeOracle) Validate(context.Context, string) ([]string, error) {
	return nil, nil
}

// ---- fixtures ----------------------------------------------------------------

// stubOwner is a PathOwner that owns nothing.
type stubOwner struct{}

// Owner implements host.PathOwner.
func (stubOwner) Owner(string) (string, bool) { return "", false }

// stubConfirmer answers every question with the configured value.
type stubConfirmer struct {
	yes       bool
	err       error
	questions []Question
}

// Confirm implements Confirmer.
func (c *stubConfirmer) Confirm(_ context.Context, q Question) (bool, error) {
	c.questions = append(c.questions, q)

	return c.yes, c.err
}

// world is one apply test world.
type world struct {
	root  string
	deps  Deps
	store *store.Store
	fakes map[host.ID]*fakeHost
}

// newWorld builds a temp-rooted Deps with the given fake hosts.
func newWorld(t *testing.T) *world {
	t.Helper()

	root := t.TempDir()

	st, err := store.Open(filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	h, err := home.New(filepath.Join(root, "home"))
	if err != nil {
		t.Fatalf("home: %v", err)
	}

	return &world{
		root:  root,
		store: st,
		fakes: map[host.ID]*fakeHost{},
		deps: Deps{
			Home:       h,
			Store:      st,
			Receipts:   receipt.NewStore(h.ReceiptsDir()),
			Journal:    receipt.OpenJournal(h.JournalPath()),
			Tombstones: receipt.NewTombstoneStore(h.TombstonesPath()),
			Hosts:      map[host.ID]host.Host{},
			Owned:      stubOwner{},
			Lock:       lock.New(),
			LockPath:   h.LockPath(),
		},
	}
}

// fake registers one fake host and returns its trash-aware handle.
func (w *world) fake(t *testing.T, id host.ID) *fakeHost {
	t.Helper()

	f := newFake(id, w.store.Trash())
	w.fakes[id] = f
	w.deps.Hosts[id] = f

	return f
}

// installAction builds an install/update action for the fixture package.
func installAction(id host.ID, kind Kind, version string, prev *receipt.Receipt) Action {
	return installActionPkg(fxPkg, id, kind, version, prev)
}

// installActionPkg builds an install/update action for one package id.
func installActionPkg(pkg string, id host.ID, kind Kind, version string, prev *receipt.Receipt) Action {
	return Action{
		Kind: kind,
		Host: id,
		Delivery: host.Delivery{
			Strategy: host.Native,
			Package: host.Package{
				ID: pkg, Version: version, Scope: fxUser,
			},
		},
		Previous: prev,
	}
}

// removeAction builds a remove action for one previously installed cell.
func removeAction(id host.ID, prev receipt.Receipt, cause, initiator string) Action {
	return Action{
		Kind: ActionRemove, Host: id, Previous: &prev, Cause: cause, Initiator: initiator,
	}
}

// run executes one Run with the world deps.
func (w *world) run(t *testing.T, ctx context.Context, plan Plan, opts Options) (Report, error) {
	t.Helper()

	return Run(ctx, w.deps, plan, opts)
}

// ---- assertions --------------------------------------------------------------

// mustFileDigest is digest.File with a test failure on error.
func mustFileDigest(t *testing.T, path string) digest.Hash {
	t.Helper()

	sum, err := digest.File(path)
	if err != nil {
		t.Fatalf("digest %s: %v", path, err)
	}

	return sum
}

// valueDigest is the canonical (json.Marshal) digest render and apply use for
// config values.
func valueDigest(t *testing.T, value any) digest.Hash {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return digest.Bytes(data)
}

// writeFixture writes one fixture file.
func writeFixture(t *testing.T, path, data string) {
	t.Helper()

	if err := fsutil.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readFixture reads one fixture file.
func readFixture(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// fileExists reports whether path exists (any type).
func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// boolText renders a boolean for call logs.
func boolText(v bool) string {
	if v {
		return "true"
	}

	return "false"
}

// snapshotTree hashes every path below root (content, kind, link target).
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		switch {
		case entry.IsDir():
			out[rel] = "dir"
		case entry.Type()&fs.ModeSymlink != 0:
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}

			out[rel] = "link:" + target
		default:
			data, readErr := os.ReadFile(path) //nolint:gosec // G304: test snapshot
			if readErr != nil {
				return readErr
			}

			sum := sha256.Sum256(data)
			out[rel] = "file:" + hex.EncodeToString(sum[:])
		}

		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	return out
}

// ---- tests -------------------------------------------------------------------

func TestRunInstallHappyPath(t *testing.T) {
	Convey("Given one installable package", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		target := filepath.Join(w.root, "skills", "one", "SKILL.md")
		f.write(fxPkg, fakeFile{Path: target, Data: "# one\n"})

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When the plan runs", func() {
			Convey("Then the cell is current and the artifact landed", func() {
				So(err, ShouldBeNil)
				So(report.Cells, ShouldHaveLength, 1)

				cell := report.Cells[0]
				So(cell.Package, ShouldEqual, fxPkg)
				So(cell.Host, ShouldEqual, fxClaude)
				So(cell.Scope, ShouldEqual, fxUser)
				So(cell.Kind, ShouldEqual, ActionInstall)
				So(cell.Strategy, ShouldEqual, lock.StrategyNative)
				So(cell.Status, ShouldEqual, StatusCurrent)
				So(cell.Version, ShouldEqual, fxVersion)
				So(readFixture(t, target), ShouldEqual, "# one\n")
			})

			Convey("Then the receipt carries the artifact and its RMA", func() {
				r, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)

				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(r.Version, ShouldEqual, fxVersion)
				So(r.Strategy, ShouldEqual, string(lock.StrategyNative))
				So(r.Artifacts, ShouldHaveLength, 1)
				So(r.Artifacts[0].Path, ShouldEqual, target)
				So(r.RMA, ShouldHaveLength, 1)
				So(r.RMA[0].Kind, ShouldEqual, receipt.OpWriteFile)
				So(r.RMA[0].Existed, ShouldBeFalse)
				So(r.RMA[0].Digest, ShouldEqual, mustFileDigest(t, target))
			})

			Convey("Then the journal holds the intent before the lock commit", func() {
				events, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)
				So(events, ShouldHaveLength, 2)
				So(events[0].Kind, ShouldEqual, receipt.EventInstall)
				So(events[0].Package, ShouldEqual, fxPkg)
				So(events[0].Version, ShouldEqual, fxVersion)
				So(events[1].Kind, ShouldEqual, receipt.EventLock)
				So(events[1].LockGeneration, ShouldEqual, 1)
				So(events[1].LockDigest.Valid(), ShouldBeTrue)
				So(events[1].LockDigest, ShouldEqual, lockDigest(t, w.deps.LockPath))
			})

			Convey("Then the lock carries one cell and no snapshot", func() {
				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldHaveLength, 1)
				So(parsed.Cells[0].Package, ShouldEqual, fxPkg)
				So(parsed.Cells[0].Version, ShouldEqual, fxVersion)
				So(parsed.Cells[0].Strategy, ShouldEqual, lock.StrategyNative)
				So(parsed.Cells[0].CapsHash, ShouldBeEmpty)
				So(parsed.Snapshot.GeneratedAt.IsZero(), ShouldBeTrue)
			})
		})
	})
}

func TestRunUpdateDropsOldArtifacts(t *testing.T) {
	Convey("Given an installed old version and a newer one", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		oldPath := filepath.Join(w.root, "skills", "old", "SKILL.md")
		newPath := filepath.Join(w.root, "skills", "new", "SKILL.md")

		writeFixture(t, oldPath, "# old\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "old", Path: oldPath, Digest: mustFileDigest(t, oldPath)}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: oldPath, Digest: mustFileDigest(t, oldPath)}},
		}

		f.write(fxPkg, fakeFile{Path: newPath, Data: "# new\n"}).probe(fxPkg, oldPath)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionUpdate, fxNext, &prev),
		}}, Options{})

		Convey("When the update runs", func() {
			Convey("Then the new artifact is installed before the old one is dropped", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[0].Version, ShouldEqual, fxNext)
				So(readFixture(t, newPath), ShouldEqual, "# new\n")
				So(fileExists(oldPath), ShouldBeFalse)

				f.mu.Lock()
				seen := f.probeOK[fxPkg]
				f.mu.Unlock()

				So(seen, ShouldBeTrue)
			})

			Convey("Then the old artifact is in the trash and the receipt replaced", func() {
				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, oldPath)
				So(entries[0].Package, ShouldEqual, fxPkg)
				So(entries[0].Host, ShouldEqual, string(fxClaude))

				r, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(r.Version, ShouldEqual, fxNext)
				So(r.Artifacts, ShouldHaveLength, 1)
				So(r.Artifacts[0].Path, ShouldEqual, newPath)

				events, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)
				So(events[0].Kind, ShouldEqual, receipt.EventUpdate)
			})
		})
	})
}

func TestRunRemoveExecutesRMA(t *testing.T) {
	Convey("Given an installed cell with files, a config key and a host install", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		filePath := filepath.Join(w.root, "skills", "one", "SKILL.md")
		settings := filepath.Join(w.root, "settings.json")

		writeFixture(t, filePath, "# one\n")
		writeFixture(t, settings, `{"model":"opus","hooks":{"PreToolUse":[]}}`+"\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{
				{Kind: "hook", Name: "hooks", Path: settings, Digest: valueDigest(t, map[string]any{"PreToolUse": []any{}})},
				{Kind: "skill", Name: "one", Path: filePath, Digest: mustFileDigest(t, filePath)},
			},
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"claude", "plugin", "uninstall", "tool@mkt"}},
				{Kind: receipt.OpWriteFile, Path: filePath, Digest: mustFileDigest(t, filePath)},
				{Kind: receipt.OpConfigKey, Path: settings, KeyPath: "hooks", Digest: valueDigest(t, map[string]any{"PreToolUse": []any{}})},
			},
		}

		f.probe(fxPkg, filePath)

		So(w.deps.Receipts.Put(prev), ShouldBeNil)
		So(w.deps.Lock.Upsert(lock.Cell{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Version: fxVersion, Strategy: lock.StrategyNative,
		}), ShouldBeNil)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), string(fxClaude)),
		}}, Options{})

		Convey("When the removal runs", func() {
			Convey("Then the cell is current and every artifact is gone or trashed", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(fileExists(filePath), ShouldBeFalse)

				raw := readFixture(t, settings)
				So(raw, ShouldContainSubstring, `"model":"opus"`)
				So(raw, ShouldNotContainSubstring, `"hooks"`)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, filePath)
				So(entries[0].Cause, ShouldEqual, string(receipt.CauseUser))

				f.mu.Lock()
				uninstalls := f.uninstalls
				fileAtUninstall := f.probeAtUn[fxPkg]
				f.mu.Unlock()

				So(uninstalls, ShouldEqual, 1)
				So(fileAtUninstall, ShouldBeFalse) // reverse order: the file went first
			})

			Convey("Then a tombstone replaces the receipt and the lock cell", func() {
				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldHaveLength, 1)
				So(tombstones[0].Package, ShouldEqual, fxPkg)
				So(tombstones[0].Host, ShouldEqual, string(fxClaude))
				So(tombstones[0].Cause, ShouldEqual, receipt.CauseUser)
				So(tombstones[0].TrashID, ShouldNotBeEmpty)
				So(tombstones[0].RemovedAt.IsZero(), ShouldBeFalse)

				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)

				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldBeEmpty)

				events, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)
				So(events[0].Kind, ShouldEqual, receipt.EventRemove)
				So(events[0].Cause, ShouldEqual, string(receipt.CauseUser))
				So(events[0].Initiator, ShouldEqual, string(fxClaude))
			})
		})
	})
}

func TestRunRemoveDriftIsHandsOff(t *testing.T) {
	Convey("Given an artifact that changed behind the receipt", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		filePath := filepath.Join(w.root, "skills", "one", "SKILL.md")
		writeFixture(t, filePath, "# drifted\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: filePath, Digest: digest.Bytes([]byte("# verger\n"))}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: filePath, Digest: digest.Bytes([]byte("# verger\n"))}},
		}

		So(w.deps.Receipts.Put(prev), ShouldBeNil)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), ""),
		}}, Options{})

		Convey("When the removal runs", func() {
			Convey("Then the cell is hands-off and nothing was removed", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusHandsOff)
				So(readFixture(t, filePath), ShouldEqual, "# drifted\n")
				So(slices.ContainsFunc(report.Cells[0].Notes, func(n string) bool { return strings.Contains(n, "hands-off") }), ShouldBeTrue)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)

				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)

				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldBeEmpty)
			})
		})
	})
}

func TestRunCollisionConfirmation(t *testing.T) {
	newCollisionWorld := func(t *testing.T) (*world, string) {
		t.Helper()

		w := newWorld(t)
		f := w.fake(t, fxClaude)

		foreign := filepath.Join(w.root, "skills", "foreign", "SKILL.md")
		writeFixture(t, foreign, "# foreign\n")

		target := filepath.Join(w.root, "skills", "one", "SKILL.md")
		f.write(fxPkg, fakeFile{Path: target, Data: "# one\n"}).collideOnce(fxPkg, foreign, 1).probe(fxPkg, foreign)

		return w, foreign
	}

	Convey("Given a foreign collision and no confirmer", t, func() {
		w, foreign := newCollisionWorld(t)
		opts := Options{Now: nil}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, opts)

		Convey("When the plan runs", func() {
			Convey("Then it fails with ErrConfirmationRequired and writes nothing", func() {
				So(errors.Is(err, ErrConfirmationRequired), ShouldBeTrue)
				So(report.Cells[0].Status, ShouldEqual, StatusFailed)
				So(readFixture(t, foreign), ShouldEqual, "# foreign\n")
				So(fileExists(filepath.Join(w.root, "skills", "one", "SKILL.md")), ShouldBeFalse)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)

				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)
			})
		})
	})

	Convey("Given a foreign collision and a refusing confirmer", t, func() {
		w, foreign := newCollisionWorld(t)
		confirmer := &stubConfirmer{}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}},
			Options{Confirm: confirmer})

		Convey("When the plan runs", func() {
			Convey("Then the cell is foreign and nothing is written", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusForeign)
				So(confirmer.questions, ShouldHaveLength, 1)
				So(confirmer.questions[0].Kind, ShouldEqual, "conflict")
				So(readFixture(t, foreign), ShouldEqual, "# foreign\n")

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})

	Convey("Given a foreign collision and a confirming confirmer", t, func() {
		w, foreign := newCollisionWorld(t)
		confirmer := &stubConfirmer{yes: true}

		target := filepath.Join(w.root, "skills", "one", "SKILL.md")

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}},
			Options{Confirm: confirmer})

		Convey("When the plan runs", func() {
			Convey("Then the collision is trashed and the install completes", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(readFixture(t, target), ShouldEqual, "# one\n")
				So(fileExists(foreign), ShouldBeFalse)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, foreign)
				So(entries[0].Cause, ShouldEqual, "conflict")
			})
		})
	})
}

func TestRunDryRunWritesNothing(t *testing.T) {
	Convey("Given a plan and two hosts", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		target := filepath.Join(w.root, "skills", "one", "SKILL.md")
		f.write(fxPkg, fakeFile{Path: target, Data: "# one\n"})

		// The home flock materializes state/.lock on its first use; warm it so
		// the snapshot measures the run, not the lock file creation.
		So(w.deps.Home.Ensure(), ShouldBeNil)

		warm, err := w.deps.Home.Lock(context.Background())
		So(err, ShouldBeNil)
		So(warm(), ShouldBeNil)

		before := snapshotTree(t, w.root)
		events := make(chan Event, 16)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}},
			Options{DryRun: true, Events: events})

		close(events)

		Convey("When the dry run completes", func() {
			Convey("Then nothing on disk changed and the report describes the plan", func() {
				So(err, ShouldBeNil)
				So(snapshotTree(t, w.root), ShouldResemble, before)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[0].Notes, ShouldContain, "dry-run")

				steps := make([]string, 0)
				for event := range events {
					steps = append(steps, event.Step)
				}

				So(steps, ShouldContain, "plan")
				So(slices.Contains(steps, "install"), ShouldBeFalse)
			})
		})
	})
}

func TestRunDryRunRemoveWritesNothing(t *testing.T) {
	Convey("Given an installed cell and a removal plan", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		target := filepath.Join(w.root, "skills", "one", "SKILL.md")
		f.write(fxPkg, fakeFile{Path: target, Data: "# one\n"})

		_, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
		So(err, ShouldBeNil)

		installed, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
		So(err, ShouldBeNil)
		So(ok, ShouldBeTrue)

		before := snapshotTree(t, w.root)
		events := make(chan Event, 16)

		report, err := w.run(t, context.Background(),
			Plan{Actions: []Action{removeAction(fxClaude, installed, string(receipt.CauseUser), "")}},
			Options{DryRun: true, Events: events})

		close(events)

		Convey("When the dry run completes", func() {
			Convey("Then the removal performed zero writes anywhere", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[0].Notes, ShouldContain, "dry-run")
				So(snapshotTree(t, w.root), ShouldResemble, before)

				journal, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)
				So(journal, ShouldHaveLength, 2) // install intent + lock commit only

				stored, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(stored.Version, ShouldEqual, fxVersion)

				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldBeEmpty)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)

				So(fileExists(target), ShouldBeTrue)

				steps := make([]string, 0)
				for event := range events {
					steps = append(steps, event.Step)
				}

				So(steps, ShouldContain, "plan")
				So(slices.Contains(steps, "remove"), ShouldBeFalse)
			})

			Convey("Then an unrelated real run does not replay the dry-run removal", func() {
				_, err := w.run(t, context.Background(), Plan{}, Options{})
				So(err, ShouldBeNil)
				So(snapshotTree(t, w.root), ShouldResemble, before)
				So(fileExists(target), ShouldBeTrue)

				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldBeEmpty)
			})

			Convey("Then a real removal afterwards behaves as usual", func() {
				report, err := w.run(t, context.Background(),
					Plan{Actions: []Action{removeAction(fxClaude, installed, string(receipt.CauseUser), string(fxClaude))}},
					Options{})

				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(fileExists(target), ShouldBeFalse)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, target)
				So(entries[0].Cause, ShouldEqual, string(receipt.CauseUser))

				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)

				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldHaveLength, 1)

				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldBeEmpty)
			})
		})
	})
}

func TestRunEmptyPlanAndConfigErrors(t *testing.T) {
	Convey("Given an empty plan", t, func() {
		w := newWorld(t)

		report, err := w.run(t, context.Background(), Plan{}, Options{})

		Convey("When it runs", func() {
			Convey("Then it is a no-op", func() {
				So(err, ShouldBeNil)
				So(report.Cells, ShouldBeEmpty)
				So(fileExists(w.deps.LockPath), ShouldBeFalse)
				So(fileExists(w.deps.Home.JournalPath()), ShouldBeFalse)
			})
		})
	})

	Convey("Given a plan naming an unregistered host", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		_, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxCodex, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When it runs", func() {
			Convey("Then it is a *ConfigError", func() {
				_, ok := errors.AsType[*ConfigError](err)
				So(ok, ShouldBeTrue)
			})
		})
	})

	Convey("Given a missing dependency", t, func() {
		cases := map[string]func(*Deps){
			"home":       func(d *Deps) { d.Home = nil },
			"store":      func(d *Deps) { d.Store = nil },
			"receipts":   func(d *Deps) { d.Receipts = nil },
			"journal":    func(d *Deps) { d.Journal = nil },
			"tombstones": func(d *Deps) { d.Tombstones = nil },
			"hosts":      func(d *Deps) { d.Hosts = nil },
			"owned":      func(d *Deps) { d.Owned = nil },
			"lock":       func(d *Deps) { d.Lock = nil },
		}

		for name, breakDep := range cases {
			Convey("When Deps."+name+" is missing", func() {
				w := newWorld(t)
				w.fake(t, fxClaude)
				breakDep(&w.deps)

				_, err := Run(context.Background(), w.deps, Plan{}, Options{})

				Convey("Then it is a *ConfigError", func() {
					_, ok := errors.AsType[*ConfigError](err)
					So(ok, ShouldBeTrue)
				})
			})
		}
	})
}

func TestRunFlockAndRelease(t *testing.T) {
	Convey("Given a home lock held elsewhere", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		unlock, err := w.deps.Home.Lock(context.Background())
		So(err, ShouldBeNil)

		defer func() { _ = unlock() }()

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		report, err := w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When Run is called", func() {
			Convey("Then the *home.LockedError is surfaced", func() {
				_, ok := errors.AsType[*home.LockedError](err)
				So(ok, ShouldBeTrue)
				So(report.Cells, ShouldBeEmpty)
			})
		})

		So(unlock(), ShouldBeNil)

		Convey("When the lock is released", func() {
			Convey("Then Run succeeds", func() {
				_, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
				So(err, ShouldBeNil)

				unlock2, err := w.deps.Home.Lock(context.Background())
				So(err, ShouldBeNil)
				So(unlock2(), ShouldBeNil)
			})
		})
	})
}

func TestRunLockGenerations(t *testing.T) {
	Convey("Given two sequential runs", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)
		f.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "a.md"), Data: "a"})

		_, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
		So(err, ShouldBeNil)

		second := installAction(fxClaude, ActionInstall, fxNext, nil)
		_, err = w.run(t, context.Background(), Plan{Actions: []Action{second}}, Options{})
		So(err, ShouldBeNil)

		events, err := w.deps.Journal.Read()
		So(err, ShouldBeNil)

		generations := make([]int, 0)
		digests := make([]digest.Hash, 0)

		for _, event := range events {
			if event.Kind == receipt.EventLock {
				generations = append(generations, event.LockGeneration)
				digests = append(digests, event.LockDigest)
			}
		}

		Convey("When the journal is read", func() {
			Convey("Then generations are 1 and 2 with the matching lock digest", func() {
				So(generations, ShouldResemble, []int{1, 2})
				So(digests[1], ShouldEqual, lockDigest(t, w.deps.LockPath))

				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Digest(), ShouldEqual, digests[1])
			})
		})
	})
}

func TestRunCancellation(t *testing.T) {
	Convey("Given a host delivery blocked by context cancellation", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		signal := make(chan struct{}, 1)
		release := make(chan struct{})

		f.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "skills", "one", "SKILL.md"), Data: "# one\n"})
		f.signal = signal
		f.blockOn(fxPkg, release)

		ctx, cancel := context.WithCancel(context.Background())

		var (
			report Report
			err    error
		)

		done := make(chan struct{})

		go func() {
			defer close(done)

			report, err = w.run(t, ctx, Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
		}()

		select {
		case <-signal:
		case <-time.After(2 * time.Second):
			t.Fatal("no real delivery started")
		}

		cancel()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}

		Convey("When the run returns", func() {
			Convey("Then the cancellation is surfaced, nothing half-written remains and the flock is free", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
				So(report.Cells, ShouldHaveLength, 1)
				So(report.Cells[0].Status, ShouldEqual, StatusFailed)

				_, ok, getErr := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeFalse)
				So(fileExists(filepath.Join(w.root, "skills", "one", "SKILL.md")), ShouldBeFalse)

				lockCtx, lockCancel := context.WithTimeout(context.Background(), time.Second)
				defer lockCancel()

				unlock, lockErr := w.deps.Home.Lock(lockCtx)
				So(lockErr, ShouldBeNil)

				_ = unlock()
			})
		})
	})
}

func TestRunParallelHosts(t *testing.T) {
	Convey("Given three hosts with a bounded parallelism", t, func() {
		w := newWorld(t)
		track := &tracker{}

		claude := w.fake(t, fxClaude)
		codex := w.fake(t, fxCodex)
		gemini := w.fake(t, host.Gemini)

		for _, fake := range []*fakeHost{claude, codex, gemini} {
			fake.track = track
			fake.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "out", string(fake.id), "a.md"), Data: string(fake.id)})
		}

		// A second action on the claude host pins per-host serialization.
		claude.write("acme/other", fakeFile{Path: filepath.Join(w.root, "out", string(fxClaude), "b.md"), Data: "b"})

		gate := make(chan struct{})
		sigClaude := make(chan struct{}, 1)
		sigCodex := make(chan struct{}, 1)

		claude.blockOn(fxPkg, gate)
		claude.signal = sigClaude

		codex.blockOn(fxPkg, gate)
		codex.signal = sigCodex

		var (
			report Report
			err    error
		)

		done := make(chan struct{})

		go func() {
			defer close(done)

			report, err = w.run(t, context.Background(), Plan{Actions: []Action{
				installAction(fxClaude, ActionInstall, fxVersion, nil),
				installAction(fxCodex, ActionInstall, fxVersion, nil),
				installAction(host.Gemini, ActionInstall, fxVersion, nil),
				installActionPkg("acme/other", fxClaude, ActionInstall, fxVersion, nil),
			}}, Options{Parallel: 2})
		}()

		for _, signal := range []chan struct{}{sigClaude, sigCodex} {
			select {
			case <-signal:
			case <-time.After(2 * time.Second):
				t.Fatal("two hosts did not start in parallel")
			}
		}

		track.mu.Lock()
		blocked := track.cur
		track.mu.Unlock()

		close(gate)

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not finish")
		}

		Convey("When the run completes", func() {
			Convey("Then the limit is respected and actions inside a host are serial", func() {
				So(err, ShouldBeNil)
				So(blocked, ShouldEqual, 2)
				So(report.Cells, ShouldHaveLength, 4)

				for _, cell := range report.Cells {
					So(cell.Status, ShouldEqual, StatusCurrent)
				}

				for _, fake := range []*fakeHost{claude, codex, gemini} {
					fake.mu.Lock()
					So(fake.maxInFlight, ShouldEqual, 1)
					fake.mu.Unlock()
				}
			})
		})
	})

	Convey("Given two hosts where one fails", t, func() {
		w := newWorld(t)

		claude := w.fake(t, fxClaude)
		codex := w.fake(t, fxCodex)

		claude.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "ok.md"), Data: "ok"})
		codex.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "bad.md"), Data: "bad"}).failVerify(fxPkg)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionInstall, fxVersion, nil),
			installAction(fxCodex, ActionInstall, fxVersion, nil),
		}}, Options{})

		Convey("When the run completes", func() {
			Convey("Then the failing host is skew and the other is current", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[1].Status, ShouldEqual, StatusSkew)
			})
		})
	})
}

func TestRunUnicodePaths(t *testing.T) {
	Convey("Given an artifact with unicode and spaces in its path", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		target := filepath.Join(w.root, "скиллы", "один ☃", "SKILL.md")
		writeFixture(t, target, "# снег\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "один", Path: target, Digest: mustFileDigest(t, target)}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: target, Digest: mustFileDigest(t, target)}},
		}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), ""),
		}}, Options{})

		Convey("When the removal runs", func() {
			Convey("Then the file round-trips through the trash", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(fileExists(target), ShouldBeFalse)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, target)

				if _, err := w.store.Trash().Restore(context.Background(), entries[0].ID); err != nil {
					t.Fatalf("restore: %v", err)
				}

				So(readFixture(t, target), ShouldEqual, "# снег\n")
			})
		})
	})
}

func TestRunAdapterFailureTaxonomy(t *testing.T) {
	Convey("Given a host with missing secrets", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		target := filepath.Join(w.root, "a.md")
		f.write(fxPkg, fakeFile{Path: target, Data: "a"})
		f.deliverErr = &host.MissingSecretsError{Names: []string{"ACME_TOKEN"}}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When the plan runs", func() {
			Convey("Then the cell needs-auth and nothing is written", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusNeedsAuth)
				So(fileExists(target), ShouldBeFalse)
				So(report.Cells[0].Notes, ShouldNotBeEmpty)
			})
		})
	})
}

// lockDigest hashes the lock file through the lock package.
func lockDigest(t *testing.T, path string) digest.Hash {
	t.Helper()

	parsed, err := lock.ParseFile(path)
	if err != nil {
		t.Fatalf("parse lock: %v", err)
	}

	return parsed.Digest()
}

func TestRunUpdateDriftRefused(t *testing.T) {
	Convey("Given an installed artifact that changed on disk", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		path := filepath.Join(w.root, "skills", "one", "SKILL.md")
		writeFixture(t, path, "# drifted\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: path, Digest: digest.Bytes([]byte("# verger\n"))}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: path, Digest: digest.Bytes([]byte("# verger\n"))}},
		}

		So(w.deps.Receipts.Put(prev), ShouldBeNil)

		f.write(fxPkg, fakeFile{Path: path, Data: "# new\n"})

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionUpdate, fxNext, &prev),
		}}, Options{})

		Convey("When the update runs", func() {
			Convey("Then it never replaces the drifted artifact and writes nothing", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusHandsOff)
				So(readFixture(t, path), ShouldEqual, "# drifted\n")

				f.mu.Lock()
				delivers := f.delivers[fxPkg]
				f.mu.Unlock()

				So(delivers, ShouldEqual, 0)

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)

				stored, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(stored.Version, ShouldEqual, fxVersion)
			})
		})
	})
}

func TestRunPartialFailureSavesSuccessfulCells(t *testing.T) {
	Convey("Given one successful and one failing host", t, func() {
		w := newWorld(t)

		claude := w.fake(t, fxClaude)
		codex := w.fake(t, fxCodex)

		claude.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "ok.md"), Data: "ok"})
		codex.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "bad.md"), Data: "bad"}).failVerify(fxPkg)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionInstall, fxVersion, nil),
			installAction(fxCodex, ActionInstall, fxVersion, nil),
		}}, Options{})

		Convey("When the run completes", func() {
			Convey("Then only the successful cell enters the lock", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[1].Status, ShouldEqual, StatusSkew)

				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldHaveLength, 1)
				So(parsed.Cells[0].Host, ShouldEqual, string(fxClaude))

				events, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)

				locks := 0

				for _, event := range events {
					if event.Kind == receipt.EventLock {
						locks++
					}
				}

				So(locks, ShouldEqual, 1)
			})
		})
	})
}

func TestRunLockSaveFailureIsFatal(t *testing.T) {
	Convey("Given a lock path under a regular file", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)
		f.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "a.md"), Data: "a"})

		blocker := filepath.Join(w.root, "blocker")
		writeFixture(t, blocker, "not a directory\n")
		w.deps.LockPath = filepath.Join(blocker, "state", "verger.lock")

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When the run commits", func() {
			Convey("Then a *LockError surfaces and the receipt is already durable", func() {
				_, ok := errors.AsType[*LockError](err)
				So(ok, ShouldBeTrue)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)

				_, stored, getErr := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(getErr, ShouldBeNil)
				So(stored, ShouldBeTrue)
			})
		})
	})
}
