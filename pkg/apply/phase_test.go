package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/store"
)

func TestVerifyFailureKeepsLastKnownGood(t *testing.T) {
	Convey("Given an update whose oracle check fails", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)
		codex := w.fake(t, fxCodex)

		path := filepath.Join(w.root, "skills", "one", "SKILL.md")
		writeFixture(t, path, "# old\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: path, Digest: mustFileDigest(t, path)}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: path, Digest: mustFileDigest(t, path)}},
		}

		f.write(fxPkg, fakeFile{Path: path, Data: "# new\n"}).failVerify(fxPkg)
		codex.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "other", "x.md"), Data: "x"})

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionUpdate, fxNext, &prev),
			installAction(fxClaude, ActionInstall, fxNext, nil),
			installAction(fxCodex, ActionInstall, fxVersion, nil),
		}}, Options{})

		Convey("When the run completes", func() {
			Convey("Then the host keeps last-known-good and the circuit breaker trips", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusSkew)
				So(report.Cells[0].Version, ShouldEqual, fxNext)
				So(slices.ContainsFunc(report.Cells[0].Notes, func(n string) bool { return strings.Contains(n, "oracle") }), ShouldBeTrue)

				So(report.Cells[1].Status, ShouldEqual, StatusSkew)
				So(slices.ContainsFunc(report.Cells[1].Notes, func(n string) bool { return strings.Contains(n, "circuit") }), ShouldBeTrue)

				So(report.Cells[2].Status, ShouldEqual, StatusCurrent)

				So(report.Breakers, ShouldHaveLength, 1)
				So(report.Breakers[0].Host, ShouldEqual, fxClaude)
				So(report.Breakers[0].Tripped, ShouldBeTrue)
				So(report.Breakers[0].Status, ShouldEqual, StatusSkew)
			})

			Convey("Then the previous bytes are restored and the new ones trashed", func() {
				So(readFixture(t, path), ShouldEqual, "# old\n")

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, path)
				So(entries[0].Cause, ShouldEqual, "rollback")

				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse) // no receipt for a failed verify; the previous plan owns the cell
			})

			Convey("Then the failed host is not retried in the same run", func() {
				f.mu.Lock()
				retries := f.delivers[fxPkg]
				f.mu.Unlock()

				So(retries, ShouldEqual, 1)
			})
		})
	})
}

func TestRMAIdempotency(t *testing.T) {
	Convey("Given a removal executed twice", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		filePath := filepath.Join(w.root, "skills", "one", "SKILL.md")
		settings := filepath.Join(w.root, "settings.json")

		writeFixture(t, filePath, "# one\n")
		writeFixture(t, settings, `{"model":"opus","hooks":{"PreToolUse":[]}}`+"\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: filePath, Digest: mustFileDigest(t, filePath)}},
			RMA: []receipt.Op{
				{Kind: receipt.OpWriteFile, Path: filePath, Digest: mustFileDigest(t, filePath)},
				{Kind: receipt.OpConfigKey, Path: settings, KeyPath: "hooks", Digest: valueDigest(t, map[string]any{"PreToolUse": []any{}})},
				{Kind: receipt.OpHostInstall, Command: []string{"claude", "plugin", "uninstall", "tool@mkt"}},
			},
		}

		plan := Plan{Actions: []Action{removeAction(fxClaude, prev, string(receipt.CauseUser), "")}}

		_, err := w.run(t, context.Background(), plan, Options{})
		So(err, ShouldBeNil)

		afterFirst := readFixture(t, settings)
		entriesFirst, err := w.store.Trash().List()
		So(err, ShouldBeNil)

		report, err := w.run(t, context.Background(), plan, Options{})

		Convey("When it runs again", func() {
			Convey("Then the second execution changes nothing", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(readFixture(t, settings), ShouldEqual, afterFirst)
				So(fileExists(filePath), ShouldBeFalse)

				entriesSecond, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entriesSecond, ShouldHaveLength, len(entriesFirst))

				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldHaveLength, 1)

				f.mu.Lock()
				uninstalls := f.uninstalls
				f.mu.Unlock()

				// The second run converges through the journal: the removal
				// intent is replayed from the tombstone, so the adapter is not
				// called again (no duplicate side effects).
				So(uninstalls, ShouldEqual, 1)
			})
		})
	})
}

func TestConfigKeyRestoreFromBackup(t *testing.T) {
	Convey("Given a config key restore with a trashed previous value", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		settings := filepath.Join(w.root, "settings.json")
		writeFixture(t, settings, `{"model":"opus","hooks":{"new":[]}}`+"\n")

		backupFile := filepath.Join(w.root, "backup.json")
		writeFixture(t, backupFile, `{"old":[]}`+"\n")

		entry, err := w.store.Trash().Put(context.Background(), backupFile,
			store.PutOptions{Package: fxPkg, Host: string(fxClaude)})
		So(err, ShouldBeNil)

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{
				{Kind: "hook", Name: "hooks", Path: settings, Digest: valueDigest(t, map[string]any{"new": []any{}})},
			},
			RMA: []receipt.Op{{
				Kind: receipt.OpConfigKey, Path: settings, KeyPath: "hooks",
				Digest: valueDigest(t, map[string]any{"new": []any{}}), Existed: true, Backup: entry.ID,
			}},
		}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), ""),
		}}, Options{})

		Convey("When the removal runs", func() {
			Convey("Then the previous value is restored from trash", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)

				var doc map[string]any
				So(json.Unmarshal([]byte(readFixture(t, settings)), &doc), ShouldBeNil)
				So(doc["hooks"], ShouldResemble, map[string]any{"old": []any{}})
				So(doc["model"], ShouldEqual, "opus")

				_, getErr := w.store.Trash().Get(entry.ID)
				_, gone := errors.AsType[*store.NotFoundError](getErr)
				So(gone, ShouldBeTrue)
			})
		})
	})
}

func TestConfigKeyDriftHandsOff(t *testing.T) {
	Convey("Given a config key that changed behind the receipt", t, func() {
		w := newWorld(t)
		w.fake(t, fxClaude)

		settings := filepath.Join(w.root, "settings.json")
		writeFixture(t, settings, `{"hooks":{"drifted":true}}`+"\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{
				{Kind: "hook", Name: "hooks", Path: settings, Digest: valueDigest(t, map[string]any{"verg": []any{}})},
			},
			RMA: []receipt.Op{{
				Kind: receipt.OpConfigKey, Path: settings, KeyPath: "hooks",
				Digest: valueDigest(t, map[string]any{"verg": []any{}}),
			}},
		}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			removeAction(fxClaude, prev, string(receipt.CauseUser), ""),
		}}, Options{})

		Convey("When the removal runs", func() {
			Convey("Then the cell is hands-off and the document is untouched", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusHandsOff)
				So(readFixture(t, settings), ShouldEqual, `{"hooks":{"drifted":true}}`+"\n")

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldBeEmpty)
			})
		})
	})
}

// configFailHost is a fake loose-style adapter: its real delivery stages the
// replaced config value as a temporary file, writes the new value and fails
// with an empty Result, as the pkg/host adapters do.
type configFailHost struct {
	id       host.ID
	trash    *store.Trash
	settings string
	oldDoc   string
	newDoc   string
}

// ID implements host.Host.
func (h *configFailHost) ID() host.ID { return h.id }

// Detect implements host.Host.
func (h *configFailHost) Detect(string) bool { return true }

// Oracle implements host.Host.
func (h *configFailHost) Oracle() host.Oracle { return noOracle{} }

// Uninstall implements host.Host.
func (h *configFailHost) Uninstall(context.Context, string, receipt.Receipt) (host.Result, error) {
	return host.Result{}, nil
}

// Deliver implements host.Host.
func (h *configFailHost) Deliver(ctx context.Context, _ string, d host.Delivery) (host.Result, error) {
	newSum, _, err := configValueDigest([]byte(h.newDoc), "hooks")
	if err != nil {
		return host.Result{}, err
	}

	result := host.Result{
		Strategy:  d.Strategy,
		Artifacts: []receipt.Artifact{{Kind: "hook", Name: "hooks", Path: h.settings, Digest: newSum}},
		RMA: []receipt.Op{{
			Kind: receipt.OpConfigKey, Path: h.settings, KeyPath: "hooks",
			Digest: newSum, Existed: true,
		}},
	}
	if d.DryRun {
		return result, nil
	}

	dir := filepath.Join(filepath.Dir(h.settings), "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // G703: test-owned temp path
		return host.Result{}, err
	}

	staged, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return host.Result{}, err
	}

	if _, err := staged.Write([]byte(h.oldDoc)); err != nil {
		return host.Result{}, err
	}

	if err := staged.Close(); err != nil {
		return host.Result{}, err
	}

	if _, err := h.trash.Put(ctx, staged.Name(), store.PutOptions{Package: d.Package.ID, Host: string(h.id)}); err != nil {
		return host.Result{}, err
	}

	if err := os.WriteFile(h.settings, []byte(h.newDoc), 0o600); err != nil { //nolint:gosec // G703: test-owned temp path
		return host.Result{}, err
	}

	return host.Result{}, &host.DeliveryError{
		Host: string(h.id), Package: d.Package.ID, Step: "install",
		Cause: errors.New("the adapter failed after the write"),
	}
}

// noOracle lists nothing.
type noOracle struct{}

// List implements host.Oracle.
func (noOracle) List(context.Context) ([]host.Installed, error) { return nil, nil }

// Validate implements host.Oracle.
func (noOracle) Validate(context.Context, string) ([]string, error) { return nil, nil }

func TestFailedInstallConfigKeyRollbackHandsOff(t *testing.T) {
	Convey("Given a failed install that replaced a config value without a recorded backup", t, func() {
		w := newWorld(t)

		settings := filepath.Join(w.root, "settings.json")
		writeFixture(t, settings, `{"hooks":{"old":[]}}`+"\n")

		w.deps.Hosts[fxClaude] = &configFailHost{
			id: fxClaude, trash: w.store.Trash(), settings: settings,
			oldDoc: `{"hooks":{"old":[]}}` + "\n", newDoc: `{"hooks":{"new":[]}}` + "\n",
		}

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionInstall, fxNext, nil),
		}}, Options{})

		Convey("When the failed install rolls back", func() {
			Convey("Then the key hands off, the new value stays and the old bytes stay in the trash", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusFailed)
				So(slices.ContainsFunc(report.Cells[0].Notes, func(n string) bool { return strings.Contains(n, "hands-off (no backup recorded)") }), ShouldBeTrue)
				So(readFixture(t, settings), ShouldEqual, `{"hooks":{"new":[]}}`+"\n")

				// The staged backup bucket carries no target path, so it cannot
				// be linked to the key; it stays in the trash, no bytes lost.
				// Documented degradation (see the package doc).
				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldNotEqual, settings)
			})
		})
	})
}

func TestHooksNotesCarriedThrough(t *testing.T) {
	Convey("Given a host that skipped hooks for consent", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		f.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "a.md"), Data: "a"})
		f.note(fxPkg, "hooks skipped: consent is pending (allow-hooks is false)")

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When the plan runs", func() {
			Convey("Then the note is carried in the cell result", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[0].Notes, ShouldContain, "hooks skipped: consent is pending (allow-hooks is false)")
			})
		})
	})
}

func TestTornJournalTail(t *testing.T) {
	Convey("Given a journal with a torn trailing line", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)
		f.write(fxPkg, fakeFile{Path: filepath.Join(w.root, "a.md"), Data: "a"})

		_, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})
		So(err, ShouldBeNil)

		file, err := os.OpenFile(w.deps.Home.JournalPath(), os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: test fixture
		So(err, ShouldBeNil)
		_, err = file.WriteString(`{"seq":`)
		So(err, ShouldBeNil)
		So(file.Close(), ShouldBeNil)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxNext, nil)}}, Options{})

		Convey("When the next run starts", func() {
			Convey("Then the tail is repaired and no phantom intent replay happens", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "recover") }), ShouldBeFalse)

				data, err := os.ReadFile(w.deps.Home.JournalPath()) //nolint:gosec // G304: test fixture
				So(err, ShouldBeNil)
				So(data[len(data)-1], ShouldEqual, '\n')

				events, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)
				So(len(events), ShouldBeGreaterThanOrEqualTo, 4)
			})
		})
	})
}

func TestCrashReplayPartialInstallRollsBack(t *testing.T) {
	Convey("Given a crashed install with only one of two artifacts on disk", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		one := filepath.Join(w.root, "skills", "one", "SKILL.md")
		two := filepath.Join(w.root, "skills", "two", "SKILL.md")

		writeFixture(t, one, "# one\n")

		intent := intentRecord{
			Action: installAction(fxClaude, ActionInstall, fxVersion, nil),
			Artifacts: []receipt.Artifact{
				{Kind: "skill", Name: "one", Path: one, Digest: digest.Bytes([]byte("# one\n"))},
				{Kind: "skill", Name: "two", Path: two, Digest: digest.Bytes([]byte("# two\n"))},
			},
			RMA: []receipt.Op{
				{Kind: receipt.OpWriteFile, Path: one, Digest: digest.Bytes([]byte("# one\n"))},
				{Kind: receipt.OpWriteFile, Path: two, Digest: digest.Bytes([]byte("# two\n"))},
			},
		}

		raw, err := json.Marshal(intent)
		So(err, ShouldBeNil)

		So(w.deps.Journal.Append(receipt.Event{
			Kind: receipt.EventInstall, Package: fxPkg, Host: string(fxClaude),
			Scope: fxUser, Version: fxVersion, Extra: map[string]json.RawMessage{intentExtraKey: raw},
		}), ShouldBeNil)

		f.write(fxPkg,
			fakeFile{Path: one, Data: "# one\n"},
			fakeFile{Path: two, Data: "# two\n"},
		)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

		Convey("When the rerun starts", func() {
			Convey("Then the partial artifact is rolled back and the install converges", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "recover") }), ShouldBeTrue)

				So(readFixture(t, one), ShouldEqual, "# one\n")
				So(readFixture(t, two), ShouldEqual, "# two\n")

				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)
				So(entries[0].Original, ShouldEqual, one)
				So(entries[0].Cause, ShouldEqual, "recovery")

				r, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(r.Artifacts, ShouldHaveLength, 2)
			})
		})
	})
}

// interruptedUpdateIntent is the journaled intent of an update that replaced
// one artifact and had not written the second one when it crashed.
func interruptedUpdateIntent(path, second string, prev *receipt.Receipt) intentRecord {
	return intentRecord{
		Action: installAction(fxClaude, ActionUpdate, fxNext, prev),
		Artifacts: []receipt.Artifact{
			{Kind: "skill", Name: "one", Path: path, Digest: digest.Bytes([]byte("# new\n"))},
			{Kind: "skill", Name: "two", Path: second, Digest: digest.Bytes([]byte("# two\n"))},
		},
		RMA: []receipt.Op{
			{Kind: receipt.OpWriteFile, Path: path, Digest: digest.Bytes([]byte("# new\n")), Existed: true},
			{Kind: receipt.OpWriteFile, Path: second, Digest: digest.Bytes([]byte("# two\n"))},
		},
	}
}

// appendIntent journals one intent record the way apply does before a write.
func appendIntent(t *testing.T, w *world, kind receipt.EventKind, version string, intent intentRecord) {
	t.Helper()

	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}

	if err := w.deps.Journal.Append(receipt.Event{
		Kind: kind, Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
		Version: version, Extra: map[string]json.RawMessage{intentExtraKey: raw},
	}); err != nil {
		t.Fatalf("append intent: %v", err)
	}
}

// crashUpdateWorld builds the interrupted-update fixture: the previous v1
// receipt over path, the journaled v2 intent whose first artifact was written
// and whose second never landed, and the rerun targets.
func crashUpdateWorld(t *testing.T) (*world, *fakeHost, string, string, receipt.Receipt) {
	t.Helper()

	w := newWorld(t)
	f := w.fake(t, fxClaude)

	path := filepath.Join(w.root, "skills", "one", "SKILL.md")
	second := filepath.Join(w.root, "skills", "two", "SKILL.md")

	writeFixture(t, path, "# old\n")

	prev := receipt.Receipt{
		Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
		Strategy: string(lock.StrategyNative), Version: fxVersion,
		Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: path, Digest: digest.Bytes([]byte("# old\n"))}},
		RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: path, Digest: digest.Bytes([]byte("# old\n"))}},
	}

	if err := w.deps.Receipts.Put(prev); err != nil {
		t.Fatalf("put receipt: %v", err)
	}

	// The journaled intent carries the dry RMA, so its Backup ids are empty.
	appendIntent(t, w, receipt.EventUpdate, fxNext, interruptedUpdateIntent(path, second, &prev))

	f.write(fxPkg, fakeFile{Path: path, Data: "# new\n"}, fakeFile{Path: second, Data: "# two\n"})

	return w, f, path, second, prev
}

func TestCrashReplayRollbackRestoresReplacedArtifact(t *testing.T) {
	Convey("Given an interrupted update whose adapter moved the original path into the trash", t, func() {
		w, f, path, second, prev := crashUpdateWorld(t)

		entry, err := w.store.Trash().Put(context.Background(), path,
			store.PutOptions{Package: fxPkg, Host: string(fxClaude)})
		So(err, ShouldBeNil)
		So(entry.Original, ShouldEqual, path)

		writeFixture(t, path, "# new\n")

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionUpdate, fxNext, &prev),
		}}, Options{})

		Convey("When the rerun replays the intent", func() {
			Convey("Then the rollback restores the original bytes and the update converges", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(report.Cells[0].Version, ShouldEqual, fxNext)

				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "recovered: rolled back") }), ShouldBeTrue)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "hands-off") }), ShouldBeFalse)
				So(readFixture(t, path), ShouldEqual, "# new\n")
				So(readFixture(t, second), ShouldEqual, "# two\n")

				f.mu.Lock()
				delivers := f.delivers[fxPkg]
				f.mu.Unlock()

				So(delivers, ShouldEqual, 1)

				stored, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(stored.Version, ShouldEqual, fxNext)
				So(stored.Artifacts, ShouldHaveLength, 2)

				// The crashed run's bytes went to the trash (cause recovery):
				// the rollback restored the original bucket instead of blindly
				// overwriting the replaced artifact. The re-delivery's backup of
				// verger's own previous version is purged on commit (NF-1).
				entries, err := w.store.Trash().List()
				So(err, ShouldBeNil)
				So(entries, ShouldHaveLength, 1)

				recovery := make([]store.Entry, 0, 1)

				for _, entry := range entries {
					if entry.Cause == causeRecovery {
						recovery = append(recovery, entry)
					}
				}

				So(recovery, ShouldHaveLength, 1)
				So(recovery[0].Original, ShouldEqual, path)

				data, err := os.ReadFile(filepath.Join(w.store.TrashDir(), recovery[0].ID, recovery[0].Stored)) //nolint:gosec // G304: test fixture
				So(err, ShouldBeNil)
				So(string(data), ShouldEqual, "# new\n")

				var pathOp receipt.Op

				for _, op := range stored.RMA {
					if op.Path == path {
						pathOp = op
					}
				}

				// The previous receipt says verger created the path: the update
				// keeps that pre-install state (NF-1).
				So(pathOp.Existed, ShouldBeFalse)
				So(pathOp.Backup, ShouldBeEmpty)
			})

			Convey("Then a later removal restores the pre-install state: the path verger created is gone", func() {
				stored, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)

				report, err := w.run(t, context.Background(), Plan{Actions: []Action{
					removeAction(fxClaude, stored, string(receipt.CauseUser), ""),
				}}, Options{})

				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(fileExists(path), ShouldBeFalse)
				So(fileExists(second), ShouldBeFalse)

				tombstones, err := w.deps.Tombstones.Load()
				So(err, ShouldBeNil)
				So(tombstones, ShouldHaveLength, 1)
			})
		})
	})
}

func TestCrashReplayRollbackRestoresStagedBackup(t *testing.T) {
	Convey("Given an interrupted update whose adapter staged the original under a cache path", t, func() {
		w, _, path, second, prev := crashUpdateWorld(t)

		oldCopy := filepath.Join(w.root, "cache", "old")
		writeFixture(t, oldCopy, "# old\n")

		entry, err := w.store.Trash().Put(context.Background(), oldCopy,
			store.PutOptions{Package: fxPkg, Host: string(fxClaude)})
		So(err, ShouldBeNil)
		So(entry.Original, ShouldEqual, oldCopy)

		writeFixture(t, path, "# new\n")

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionUpdate, fxNext, &prev),
		}}, Options{})

		Convey("When the rerun replays the intent", func() {
			Convey("Then the content-matched backup restores the original and converges", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "hands-off") }), ShouldBeFalse)
				So(readFixture(t, path), ShouldEqual, "# new\n")
				So(readFixture(t, second), ShouldEqual, "# two\n")

				stored, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(stored.Version, ShouldEqual, fxNext)
			})
		})
	})
}

func TestCrashReplayRollbackHandsOffIsNotReportedRecovered(t *testing.T) {
	Convey("Given an interrupted update whose replaced bytes are not in the trash", t, func() {
		w := newWorld(t)
		f := w.fake(t, fxClaude)

		path := filepath.Join(w.root, "skills", "one", "SKILL.md")
		second := filepath.Join(w.root, "skills", "two", "SKILL.md")

		// The crashed run's byte is on disk; the original bucket is missing
		// and the second artifact never landed (partial install).
		writeFixture(t, path, "# new\n")

		prev := receipt.Receipt{
			Package: fxPkg, Host: string(fxClaude), Scope: fxUser,
			Strategy: string(lock.StrategyNative), Version: fxVersion,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: path, Digest: digest.Bytes([]byte("# old\n"))}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: path, Digest: digest.Bytes([]byte("# old\n"))}},
		}

		So(w.deps.Receipts.Put(prev), ShouldBeNil)

		intent := interruptedUpdateIntent(path, second, &prev)
		appendIntent(t, w, receipt.EventUpdate, fxNext, intent)

		// Nothing left to deliver: the rollback hands off (no backup), then
		// the planned update hands off against the drifted artifact.
		f.write(fxPkg)

		report, err := w.run(t, context.Background(), Plan{Actions: []Action{
			installAction(fxClaude, ActionUpdate, fxNext, &prev),
		}}, Options{})

		Convey("When the rerun replays the intent", func() {
			Convey("Then the stale rollback is reported as incomplete, not recovered", func() {
				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusHandsOff)
				So(readFixture(t, path), ShouldEqual, "# new\n")

				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "hands-off") }), ShouldBeTrue)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "could not be fully rolled back") }), ShouldBeTrue)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "recovered: rolled back") }), ShouldBeFalse)
			})
		})
	})
}

func TestIntentPayloadRoundTrip(t *testing.T) {
	Convey("Given an action carrying components, MCP servers and hooks", t, func() {
		action := installAction(fxClaude, ActionInstall, fxVersion, nil)
		action.Delivery.Package.Components = []manifest.Component{
			{Kind: manifest.KindSkill, Name: "one", Path: "skills/one", Digest: digest.Bytes([]byte("x"))},
		}
		action.Delivery.Package.MCP = []manifest.MCPServer{
			{
				Name: "fs", Transport: "stdio", Command: []string{"node", "${PLUGIN_ROOT}/s.js"},
				Env: map[string]string{"TOKEN": "{secret:T}"},
			},
		}
		action.Delivery.Package.Hooks = []manifest.Hook{
			{Event: manifest.EventPreTool, Matcher: "*", Command: "echo hi", Origin: manifest.FormatClaude},
		}

		intent := intentRecord{
			Action:    action,
			Artifacts: []receipt.Artifact{{Kind: "skill", Name: "one", Path: "/tmp/one", Digest: digest.Bytes([]byte("x"))}},
			RMA:       []receipt.Op{{Kind: receipt.OpWriteFile, Path: "/tmp/one", Digest: digest.Bytes([]byte("x"))}},
		}

		raw, err := json.Marshal(intent)
		So(err, ShouldBeNil)

		var back intentRecord
		So(json.Unmarshal(raw, &back), ShouldBeNil)

		Convey("When the intent round-trips through the journal", func() {
			Convey("Then the whole action survives", func() {
				So(back, ShouldResemble, intent)
			})
		})
	})
}

// ---- crash replay across processes ------------------------------------------

// Crash helper protocol: the parent re-runs the test binary, the child builds
// the same world, arms the crash seam and dies between install and commit.
const (
	crashHelperEnv  = "VERGER_APPLY_CRASH_HELPER"
	crashHelperRoot = "VERGER_APPLY_CRASH_ROOT"
	crashHelperMode = "VERGER_APPLY_CRASH_MODE" // install|receipt
)

// TestCrashHelperProcess is the child side of the crash-replay tests.
func TestCrashHelperProcess(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "1" {
		return
	}

	root := os.Getenv(crashHelperRoot)

	w := worldAt(t, root)
	f := w.fake(t, fxClaude)
	f.write(fxPkg, fakeFile{Path: filepath.Join(root, "skills", "one", "SKILL.md"), Data: "# one\n"})

	switch os.Getenv(crashHelperMode) {
	case "receipt":
		crashAfterReceipt = func() { panic("kill") }
	default:
		crashAfterInstall = func() { panic("kill") }
	}

	_, _ = w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

	t.Fatal("crash helper survived the seam")
}

// crasher runs the helper process and returns its combined output.
func crasher(t *testing.T, root, mode string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashHelperProcess$") //nolint:gosec // G204: the test binary re-runs itself

	cmd.Env = append(os.Environ(), crashHelperEnv+"=1", crashHelperRoot+"="+root, crashHelperMode+"="+mode)

	out, _ := cmd.CombinedOutput()

	return string(out)
}

// crashWorld roots a world at an explicit directory.
func worldAt(t *testing.T, root string) *world {
	t.Helper()

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

func TestCrashReplayForwardCommit(t *testing.T) {
	Convey("Given a process killed between install and receipt", t, func() {
		root := t.TempDir()
		out := crasher(t, root, "install")

		Convey("When the next run replays the intent", func() {
			target := filepath.Join(root, "skills", "one", "SKILL.md")

			Convey("Then the crashed state has the artifact but no receipt", func() {
				So(out, ShouldContainSubstring, "panic: kill")
				So(readFixture(t, target), ShouldEqual, "# one\n")

				w := worldAt(t, root)
				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeFalse)

				data, err := os.ReadFile(w.deps.Home.JournalPath()) //nolint:gosec // G304: test fixture
				So(err, ShouldBeNil)
				So(data[len(data)-1], ShouldEqual, '\n')
			})

			Convey("Then the rerun commits the cell without delivering twice", func() {
				w := worldAt(t, root)
				f := w.fake(t, fxClaude) // no targets: any real delivery would fail

				report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

				So(err, ShouldBeNil)
				So(report.Cells, ShouldHaveLength, 1)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "recover") }), ShouldBeTrue)

				f.mu.Lock()
				delivers := f.delivers[fxPkg]
				f.mu.Unlock()

				So(delivers, ShouldEqual, 0)

				r, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(r.Artifacts, ShouldHaveLength, 1)
				So(readFixture(t, target), ShouldEqual, "# one\n")

				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldHaveLength, 1)

				events, err := w.deps.Journal.Read()
				So(err, ShouldBeNil)

				installs := 0

				for _, event := range events {
					if event.Kind == receipt.EventInstall {
						installs++
					}
				}

				So(installs, ShouldEqual, 1)
			})
		})
	})
}

func TestCrashReplayAfterReceipt(t *testing.T) {
	Convey("Given a process killed after the receipt but before the lock commit", t, func() {
		root := t.TempDir()
		out := crasher(t, root, "receipt")

		Convey("When the next run replays the intent", func() {
			Convey("Then the receipt exists but the lock does not", func() {
				So(out, ShouldContainSubstring, "panic: kill")

				w := worldAt(t, root)

				_, ok, err := w.deps.Receipts.Get(fxPkg, string(fxClaude), fxUser)
				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(fileExists(w.deps.LockPath), ShouldBeFalse)
			})

			Convey("Then the rerun reconstructs the lock cell without a second delivery", func() {
				w := worldAt(t, root)
				f := w.fake(t, fxClaude)

				report, err := w.run(t, context.Background(), Plan{Actions: []Action{installAction(fxClaude, ActionInstall, fxVersion, nil)}}, Options{})

				So(err, ShouldBeNil)
				So(report.Cells[0].Status, ShouldEqual, StatusCurrent)
				So(slices.ContainsFunc(report.Notes, func(n string) bool { return strings.Contains(n, "recover") }), ShouldBeTrue)

				f.mu.Lock()
				delivers := f.delivers[fxPkg]
				f.mu.Unlock()

				So(delivers, ShouldEqual, 0)

				parsed, err := lock.ParseFile(w.deps.LockPath)
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldHaveLength, 1)
			})
		})
	})
}
