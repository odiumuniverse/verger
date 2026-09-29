package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/consent"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/verger"
)

func TestInstallAppliesWithYes(t *testing.T) {
	Convey("Given a local fixture package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		ref := w.fixture(t)

		stdout, err := w.run("install", ref, "-y", "--json")

		Convey("When install runs with -y", func() {
			Convey("Then the package lands, the receipt and lock are written", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# installed\n")

				receipts := receipt.NewStore(filepath.Join(w.homeDir, "state", "receipts"))
				rec, ok, err := receipts.Get("local:caveman", "claude", receipt.ScopeUser)

				So(err, ShouldBeNil)
				So(ok, ShouldBeTrue)
				So(rec.Version, ShouldEqual, "1.2.3")
				So(rec.Strategy, ShouldEqual, "loose")

				parsed, err := lock.ParseFile(filepath.Join(w.homeDir, "verger.lock"))
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldHaveLength, 1)
				So(parsed.Cells[0].Package, ShouldEqual, "local:caveman")

				spec := readWorldFile(t, filepath.Join(w.homeDir, "verger.toml"))
				So(spec, ShouldContainSubstring, `id = 'local:caveman'`)
			})

			Convey("Then the JSON report carries the cell", func() {
				var doc struct {
					Cells []struct {
						Package  string `json:"package"`
						Host     string `json:"host"`
						Scope    string `json:"scope"`
						Status   string `json:"status"`
						Version  string `json:"version"`
						Strategy string `json:"strategy"`
						Kind     string `json:"kind"`
					} `json:"cells"`
					Notes []string `json:"notes"`
				}

				So(json.Unmarshal([]byte(stdout), &doc), ShouldBeNil)
				So(doc.Cells, ShouldHaveLength, 1)
				So(doc.Cells[0].Package, ShouldEqual, "local:caveman")
				So(doc.Cells[0].Host, ShouldEqual, "claude")
				So(doc.Cells[0].Scope, ShouldEqual, "user")
				So(doc.Cells[0].Status, ShouldEqual, "delivered")
				So(doc.Cells[0].Version, ShouldEqual, "1.2.3")
				So(doc.Cells[0].Strategy, ShouldEqual, "loose")
				So(doc.Cells[0].Kind, ShouldEqual, "install")
			})
		})
	})
}

func TestInstallNoTTYRequiresConfirmation(t *testing.T) {
	Convey("Given a fixture with hooks and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		ref := w.fixture(t)

		before := snapshot(t, w.root)

		stdout, err := w.run("install", ref)

		Convey("When install runs without -y", func() {
			Convey("Then it fails with ErrConfirmationRequired and writes nothing", func() {
				So(errors.Is(err, ErrConfirmationRequired), ShouldBeTrue)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(stdout, ShouldContainSubstring, "caveman")
				So(fileExists(target), ShouldBeFalse)
				So(fileExists(filepath.Join(w.homeDir, "verger.toml")), ShouldBeFalse)
			})
		})
	})
}

func TestInstallNoHooksNoTTYRequiresConfirmation(t *testing.T) {
	Convey("Given a hooks-less fixture and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		ref := w.noHooksFixture(t)

		before := snapshot(t, w.root)

		stdout, err := w.run("install", ref)

		Convey("When install runs without -y", func() {
			Convey("Then the plan is printed and the run stops before any write", func() {
				So(errors.Is(err, ErrConfirmationRequired), ShouldBeTrue)
				So(exitCode(err), ShouldEqual, 1)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(stdout, ShouldContainSubstring, "nohooks")
				So(fileExists(target), ShouldBeFalse)
				So(fileExists(filepath.Join(w.homeDir, "verger.toml")), ShouldBeFalse)
			})
		})

		Convey("When install runs with -y", func() {
			Convey("Then the defaults apply and the package lands", func() {
				_, err := w.run("install", ref, "-y")
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# installed\n")
			})
		})

		Convey("When install --dry-run runs without -y", func() {
			Convey("Then the preview is allowed and still writes nothing", func() {
				// The dry-run still takes the home flock; warm it so the
				// snapshot measures the run, not the lock file creation.
				w.warmHome(t)

				warmed := snapshot(t, w.root)

				stdout, err := w.run("install", ref, "--dry-run")
				So(err, ShouldBeNil)
				So(snapshot(t, w.root), ShouldResemble, warmed)
				So(stdout, ShouldContainSubstring, "nohooks")
				So(fileExists(target), ShouldBeFalse)
			})
		})
	})
}

func TestInstallHooksModesAndDryRun(t *testing.T) {
	Convey("Given the hooks modes", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")

		Convey("When --hooks no is used", func() {
			_, err := w.run("install", w.fixture(t), "-y", "--hooks", "no")

			Convey("Then hooks are not delivered", func() {
				So(err, ShouldBeNil)
				So(w.fake.lastAllowHooks, ShouldBeFalse)
			})
		})

		Convey("When --hooks yes is used", func() {
			_, err := w.run("install", w.fixture(t), "-y", "--hooks", "yes")

			Convey("Then hooks are delivered", func() {
				So(err, ShouldBeNil)
				So(w.fake.lastAllowHooks, ShouldBeTrue)
			})
		})
	})

	Convey("Given a dry run", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")

		ref := w.fixture(t)

		// The home flock materializes state/.lock on first use; warm it so the
		// snapshot measures the install, not the lock file creation.
		w.warmHome(t)

		before := snapshot(t, w.root)

		stdout, err := w.run("install", ref, "-y", "--dry-run", "--json")

		Convey("When install --dry-run runs", func() {
			Convey("Then nothing is written and the plan is reported", func() {
				So(err, ShouldBeNil)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(stdout, ShouldContainSubstring, `"status":"delivered"`)
			})
		})
	})
}

func TestInstallHostSelection(t *testing.T) {
	Convey("Given two detected hosts", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		codex := newFakeHost("codex")
		w.hosts = append(w.hosts, codex)

		w.target(t, "SKILL.md", "# installed\n")

		stdout, err := w.run("install", w.fixture(t), "-y", "--hosts", "claude", "--json")

		Convey("When --hosts limits the run", func() {
			Convey("Then only the selected host is planned", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, `"host":"claude"`)
				So(stdout, ShouldNotContainSubstring, `"host":"codex"`)

				codex.mu.Lock()
				delivers := codex.delivers
				codex.mu.Unlock()

				So(delivers, ShouldEqual, 0)
			})
		})
	})

	Convey("Given an unknown --hosts value", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("install", w.fixture(t), "-y", "--hosts", "nope")

		Convey("Then it is a *UsageError", func() {
			_, ok := errors.AsType[*UsageError](err)
			So(ok, ShouldBeTrue)
		})
	})
}

func TestHostSelectionAcceptsGemini(t *testing.T) {
	Convey("Given a registered gemini adapter", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := filepath.Join(w.root, "host", "gemini.md")

		gemini := newFakeHost(host.Gemini)
		gemini.files = []fakeFile{{Path: target, Data: "# gemini\n"}}
		w.hosts = []host.Host{gemini}

		_, err := w.run("install", w.fixture(t), "-y", "--hosts", "gemini")

		Convey("When install --hosts gemini runs", func() {
			Convey("Then gemini is a valid target and receives the delivery", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# gemini\n")
			})
		})
	})

	Convey("Given a valid host id without a registered adapter", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		_, err := w.run("install", "./fixture", "-y", "--hosts", "gemini")

		Convey("When --hosts gemini runs", func() {
			Convey("Then it is not rejected as an unknown host", func() {
				So(err, ShouldNotBeNil)

				_, isUsage := errors.AsType[*UsageError](err)
				So(isUsage, ShouldBeFalse)
			})
		})
	})

	Convey("Given a genuinely unknown host", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		_, err := w.run("install", "./fixture", "-y", "--hosts", "bogus")

		Convey("When --hosts bogus runs", func() {
			Convey("Then it stays a typed unknown-host usage error", func() {
				_, isUsage := errors.AsType[*UsageError](err)
				So(isUsage, ShouldBeTrue)
			})
		})
	})
}

func TestHostFactoryIncludesLandedAdapters(t *testing.T) {
	Convey("Given the default host factory", t, func() {
		// Pin the temp home; the factory itself gets no injected hosts.
		newWorld(t)

		client, err := verger.Open(context.Background())
		So(err, ShouldBeNil)

		ids := make([]string, 0)

		for _, adapter := range newApp(Options{}).hosts(client) {
			ids = append(ids, string(adapter.ID()))
		}

		Convey("When the adapters are built", func() {
			Convey("Then every landed Ф1 adapter is wired", func() {
				So(ids, ShouldContain, "claude")
				So(ids, ShouldContain, "codex")
				So(ids, ShouldContain, "gemini")
			})
		})
	})
}

func TestRemoveAndRestore(t *testing.T) {
	Convey("Given an installed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")

		stdout, err := w.run("remove", "caveman", "-y", "--json")

		Convey("When remove runs", func() {
			Convey("Then the artifact is trashed and a tombstone written", func() {
				So(err, ShouldBeNil)
				So(fileExists(target), ShouldBeFalse)
				So(stdout, ShouldContainSubstring, `"status":"delivered"`)

				receipts := receipt.NewStore(filepath.Join(w.homeDir, "state", "receipts"))
				_, ok, getErr := receipts.Get("local:caveman", "claude", receipt.ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeFalse)

				tombstones := receipt.NewTombstoneStore(filepath.Join(w.homeDir, "state", "tombstones.json"))
				list, loadErr := tombstones.Load()
				So(loadErr, ShouldBeNil)
				So(list, ShouldHaveLength, 1)
				So(list[0].Cause, ShouldEqual, receipt.CauseUser)
			})

			Convey("Then a second remove is a note, not an error", func() {
				_, secondErr := w.run("remove", "caveman", "-y", "--json")
				So(secondErr, ShouldBeNil)
			})
		})
	})
}

func TestRemoveNoTTYRequiresConfirmation(t *testing.T) {
	Convey("Given an installed hooks-less package and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.noHooksFixture(t), "-y")

		before := snapshot(t, w.root)

		stdout, err := w.run("remove", "nohooks")

		Convey("When remove runs without -y", func() {
			Convey("Then the plan is printed, the run fails and nothing is removed", func() {
				So(errors.Is(err, ErrConfirmationRequired), ShouldBeTrue)
				So(exitCode(err), ShouldEqual, 1)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(stdout, ShouldContainSubstring, "nohooks")
				So(readWorldFile(t, target), ShouldEqual, "# installed\n")
			})
		})

		Convey("When remove runs with -y", func() {
			Convey("Then the removal proceeds as usual", func() {
				_, err := w.run("remove", "nohooks", "-y")
				So(err, ShouldBeNil)
				So(fileExists(target), ShouldBeFalse)
			})
		})
	})
}

func TestRestoreRoundTrip(t *testing.T) {
	Convey("Given a removed package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		w.mustRun(t, "install", w.fixture(t), "-y")
		w.mustRun(t, "remove", "caveman", "-y")

		stdout, err := w.run("restore", "caveman", "-y", "--json")

		Convey("When restore runs", func() {
			Convey("Then the artifact is back from the trash", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# installed\n")
				So(stdout, ShouldContainSubstring, "restored")
			})
		})
	})
}

func TestSyncReconcilesSpec(t *testing.T) {
	Convey("Given a spec with one package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		// The spec and the package it points at belong together: a relative
		// source resolves against the spec's own directory, so a fixture
		// parked elsewhere only worked while the process happened to sit
		// there. That accident is what broke the vault case.
		w.fixtureIn(t, w.homeDir)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[source]]
name = "local"
url = "./fixture"

[[package]]
id = "local:caveman"
`)

		stdout, err := w.run("sync", "-y", "--json")

		Convey("When sync runs", func() {
			Convey("Then the missing package is installed from the spec", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# installed\n")
				// The cell arrived from the lock with no receipt of its own, so
				// it is `restored`, not `delivered`: nothing was installed
				// from a fetch on this machine.
				So(stdout, ShouldContainSubstring, `"status":"restored"`)

				parsed, err := lock.ParseFile(filepath.Join(w.homeDir, "verger.lock"))
				So(err, ShouldBeNil)
				So(parsed.Cells, ShouldHaveLength, 1)
			})
		})
	})

	Convey("Given a spec whose package is dropped", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		// Next to the spec, which is what a relative source means.
		w.fixtureIn(t, w.homeDir)

		specPath := filepath.Join(w.homeDir, "verger.toml")
		writeWorldFile(t, specPath, `schema = 1

[[source]]
name = "local"
url = "./fixture"

[[package]]
id = "local:caveman"
`)

		w.mustRun(t, "sync", "-y")

		writeWorldFile(t, specPath, "schema = 1\n")

		stdout, err := w.run("sync", "-y", "--json")

		Convey("When sync runs without the package", func() {
			Convey("Then the receipt is removed and the artifact trashed", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldContainSubstring, `"kind":"remove"`)
				So(fileExists(target), ShouldBeFalse)
			})
		})
	})

	Convey("Given a spec and --locked", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")
		// Next to the spec, which is what a relative source means.
		w.fixtureIn(t, w.homeDir)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[source]]
name = "local"
url = "./fixture"

[[package]]
id = "local:caveman"
`)

		_, err := w.run("sync", "-y", "--locked", "--json")

		Convey("When the result would change the lock", func() {
			Convey("Then it fails with *LockedError and writes nothing", func() {
				_, ok := errors.AsType[*LockedError](err)
				So(ok, ShouldBeTrue)
				So(fileExists(filepath.Join(w.homeDir, "verger.lock")), ShouldBeFalse)
			})
		})
	})
}

func TestSyncNoTTYRequiresConfirmation(t *testing.T) {
	Convey("Given a spec with a hooks-less package and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		target := w.target(t, "SKILL.md", "# installed\n")
		// Next to the spec, which is what a relative source means.
		w.noHooksFixtureIn(t, w.homeDir)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[source]]
name = "local"
url = "./nohooks"

[[package]]
id = "local:nohooks"
`)

		before := snapshot(t, w.root)

		stdout, err := w.run("sync")

		Convey("When sync runs without -y", func() {
			Convey("Then the plan is printed, the run fails and nothing is installed", func() {
				So(errors.Is(err, ErrConfirmationRequired), ShouldBeTrue)
				So(exitCode(err), ShouldEqual, 1)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(stdout, ShouldContainSubstring, "nohooks")
				So(fileExists(target), ShouldBeFalse)
				So(fileExists(filepath.Join(w.homeDir, "verger.lock")), ShouldBeFalse)
			})
		})

		Convey("When sync runs with -y", func() {
			Convey("Then the missing package is installed", func() {
				_, err := w.run("sync", "-y")
				So(err, ShouldBeNil)
				So(readWorldFile(t, target), ShouldEqual, "# installed\n")
			})
		})
	})
}

func TestAdoptAddsSpecEntry(t *testing.T) {
	Convey("Given a host that already lists a package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		installed := filepath.Join(w.root, "host", "adopted")
		writeWorldFile(t, installed, "x")

		w.fake.listed = []host.Installed{{Name: "caveman", Version: "1.2.3", Path: installed}}

		stdout, err := w.run("adopt", "claude:caveman", "-y", "--json")

		Convey("When adopt runs", func() {
			Convey("Then the spec gains adopted_from and no hooks are delivered", func() {
				So(err, ShouldBeNil)

				spec := readWorldFile(t, filepath.Join(w.homeDir, "verger.toml"))
				So(spec, ShouldContainSubstring, `adopted_from = 'claude'`)
				So(spec, ShouldContainSubstring, `id = 'caveman'`)
				So(w.fake.lastAllowHooks, ShouldBeFalse)
				So(stdout, ShouldContainSubstring, "adopted")
			})
		})
	})

	Convey("Given a host that does not list the package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		_, err := w.run("adopt", "claude:missing", "-y")

		Convey("Then the oracle miss is reported", func() {
			So(err, ShouldNotBeNil)
		})
	})
}

func TestTrustDryRunWritesNothing(t *testing.T) {
	Convey("Given a project spec and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.project)

		writeWorldFile(t, filepath.Join(w.project, "verger.toml"), "schema = 1\n")

		before := snapshot(t, w.root)

		stdout, err := w.run("trust", "--dry-run")

		Convey("When trust --dry-run runs without -y", func() {
			Convey("Then the preview is printed and no state file is created", func() {
				So(err, ShouldBeNil)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(fileExists(filepath.Join(w.homeDir, "state", "trust.json")), ShouldBeFalse)
				So(stdout, ShouldContainSubstring, "would trust")
			})
		})
	})

	Convey("Given a trusted project", t, func() {
		w := newWorld(t)
		w.chdir(t, w.project)

		writeWorldFile(t, filepath.Join(w.project, "verger.toml"), "schema = 1\n")
		w.mustRun(t, "trust", "-y")

		before := snapshot(t, w.root)

		stdout, err := w.run("untrust", "--dry-run")

		Convey("When untrust --dry-run runs", func() {
			Convey("Then the trust record is untouched", func() {
				So(err, ShouldBeNil)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(stdout, ShouldContainSubstring, "would untrust")
			})
		})
	})
}

func TestApproveAndRevokeHooks(t *testing.T) {
	Convey("Given a fixture with hooks", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")

		ref := w.fixture(t)

		stdout, approveErr := w.run("approve", "local:caveman", ref)

		Convey("When approve runs", func() {
			Convey("Then the hash is recorded and the install proceeds with -y", func() {
				So(approveErr, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "approved hooks for local:caveman")

				_, installErr := w.run("install", ref, "-y")
				So(installErr, ShouldBeNil)

				receipts := receipt.NewStore(filepath.Join(w.homeDir, "state", "receipts"))
				_, ok, getErr := receipts.Get("local:caveman", "claude", receipt.ScopeUser)
				So(getErr, ShouldBeNil)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When revoke runs after approve", func() {
			_, revokeErr := w.run("revoke", "local:caveman")

			Convey("Then a detached install asks again", func() {
				So(revokeErr, ShouldBeNil)

				_, installErr := w.run("install", ref)
				So(errors.Is(installErr, ErrConfirmationRequired), ShouldBeTrue)
			})
		})
	})
}

func TestApproveRevokeDryRunWritesNothing(t *testing.T) {
	Convey("Given a fixture with hooks and no TTY", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)
		w.target(t, "SKILL.md", "# installed\n")

		ref := w.fixture(t)

		before := snapshot(t, w.root)

		stdout, err := w.run("approve", "local:caveman", ref, "--dry-run")

		Convey("When approve --dry-run runs", func() {
			Convey("Then the preview is printed and no consent is written", func() {
				So(err, ShouldBeNil)
				So(snapshot(t, w.root), ShouldResemble, before)
				So(fileExists(filepath.Join(w.homeDir, "state", "consent.json")), ShouldBeFalse)
				So(stdout, ShouldContainSubstring, "would approve")
			})
		})

		Convey("When a real approve is followed by revoke --dry-run", func() {
			Convey("Then the approval record survives the preview", func() {
				_, approveErr := w.run("approve", "local:caveman", ref)
				So(approveErr, ShouldBeNil)
				So(fileExists(filepath.Join(w.homeDir, "state", "consent.json")), ShouldBeTrue)

				approved := snapshot(t, w.root)

				stdout, err := w.run("revoke", "local:caveman", "--dry-run")
				So(err, ShouldBeNil)
				So(snapshot(t, w.root), ShouldResemble, approved)
				So(stdout, ShouldContainSubstring, "would revoke")

				store := consent.NewStore(filepath.Join(w.homeDir, "state", "consent.json"))
				So(store.Load(), ShouldBeNil)

				_, ok := store.Hooks("local:caveman")
				So(ok, ShouldBeTrue)
			})
		})
	})
}

func TestUpdateAndPin(t *testing.T) {
	Convey("Given a spec package", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), `schema = 1

[[package]]
id = "caveman"
`)

		_, err := w.run("pin", "caveman@2.0.0")

		Convey("When pin runs", func() {
			Convey("Then the spec pin is stored and unpin clears it", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, filepath.Join(w.homeDir, "verger.toml")), ShouldContainSubstring, `version = '2.0.0'`)

				_, unpinErr := w.run("unpin", "caveman")
				So(unpinErr, ShouldBeNil)
				So(readWorldFile(t, filepath.Join(w.homeDir, "verger.toml")), ShouldNotContainSubstring, `version = '2.0.0'`)
			})
		})
	})
}

func TestProjectScopeRequiresTrust(t *testing.T) {
	Convey("Given a project spec that is not trusted", t, func() {
		w := newWorld(t)
		w.chdir(t, w.project)
		w.target(t, "SKILL.md", "# installed\n")
		w.fixtureIn(t, w.project)

		writeWorldFile(t, filepath.Join(w.project, "verger.toml"), `schema = 1

[[source]]
name = "local"
url = "./fixture"

[[package]]
id = "local:caveman"
`)

		_, err := w.run("sync", "--project", "-y", "--json")

		Convey("When sync --project runs", func() {
			Convey("Then it fails with *TrustError and writes nothing", func() {
				target, ok := errors.AsType[*TrustError](err)
				So(ok, ShouldBeTrue)
				So(target.Path, ShouldContainSubstring, "verger.toml")
				So(fileExists(filepath.Join(w.project, "verger.lock")), ShouldBeFalse)
			})
		})

		Convey("When trust runs first", func() {
			_, trustErr := w.run("trust", "-y")

			Convey("Then the project becomes installable", func() {
				So(trustErr, ShouldBeNil)

				client, openErr := verger.Open(context.Background(), w.options().openOpts...)
				So(openErr, ShouldBeNil)

				store := consent.NewTrustStore(client.Home().TrustPath())
				So(store.Load(), ShouldBeNil)

				records, recErr := store.Records()
				So(recErr, ShouldBeNil)
				So(records, ShouldHaveLength, 1)
				So(records[0].Project, ShouldContainSubstring, "project")

				_, syncErr := w.run("sync", "--project", "-y", "--json")
				So(syncErr, ShouldBeNil)
				So(fileExists(filepath.Join(w.project, "verger.lock")), ShouldBeTrue)
				So(fileExists(filepath.Join(w.project, ".verger", "state", "receipts")), ShouldBeTrue)
			})
		})
	})
}

func TestSecretSetRemoveNeverLeaks(t *testing.T) {
	Convey("Given a secret value on stdin", t, func() {
		w := newWorld(t)
		w.in = "sup3r-s3cret\n"

		stdout, err := w.run("secret", "set", "ACME_TOKEN")

		Convey("When the secret is stored", func() {
			Convey("Then the value never appears in the output", func() {
				So(err, ShouldBeNil)
				So(stdout, ShouldNotContainSubstring, "sup3r-s3cret")
				So(w.err.String(), ShouldNotContainSubstring, "sup3r-s3cret")

				_, rmErr := w.run("secret", "rm", "ACME_TOKEN")
				So(rmErr, ShouldBeNil)
			})
		})
	})
}

func TestSourceEditsSpec(t *testing.T) {
	Convey("Given a spec", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		writeWorldFile(t, filepath.Join(w.homeDir, "verger.toml"), "schema = 1\n")

		_, err := w.run("source", "add", "github:acme/market")

		Convey("When a source is added", func() {
			Convey("Then it lands in the spec and lists back", func() {
				So(err, ShouldBeNil)
				So(readWorldFile(t, filepath.Join(w.homeDir, "verger.toml")), ShouldContainSubstring, "acme/market")

				stdout, listErr := w.run("source", "list")
				So(listErr, ShouldBeNil)
				So(stdout, ShouldContainSubstring, "acme/market")

				_, rmErr := w.run("source", "rm", "market")
				So(rmErr, ShouldBeNil)
			})
		})
	})
}

func TestHomeIsolationHelper(t *testing.T) {
	Convey("Given the world helper", t, func() {
		w := newWorld(t)

		Convey("When the home is resolved", func() {
			Convey("Then it stays inside the temp root", func() {
				h, err := home.Discover(home.WithEnv(func(string) string { return "" }), home.WithUserHome(filepath.Join(w.root, "user")))
				So(err, ShouldBeNil)
				So(strings.HasPrefix(h.Root(), w.root), ShouldBeTrue)
			})
		})
	})
}
