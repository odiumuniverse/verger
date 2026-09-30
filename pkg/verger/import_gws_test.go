package verger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
)

// brokenOracle is a host whose own listing fails, so the "one host cannot
// answer" path can be exercised without breaking a real adapter.
type brokenOracle struct{}

func (brokenOracle) List(context.Context) ([]host.Installed, error) {
	return nil, errors.New("plugin list failed: no such command")
}

func (brokenOracle) Validate(context.Context, string) ([]string, error) { return nil, nil }

// silentHost is the injected fake with its oracle swapped out.
type silentHost struct {
	*fakeHost
	oracle host.Oracle
}

func (h silentHost) Oracle() host.Oracle { return h.oracle }

// importHost is one adapter an import test injects: what it lists, or that it
// cannot answer at all.
type importHost struct {
	id        host.ID
	installed []host.Installed
	silent    bool
}

func (h importHost) build() host.Host {
	base := &fakeHost{id: h.id}
	base.installed = h.installed

	if h.silent {
		return silentHost{fakeHost: base, oracle: brokenOracle{}}
	}

	return base
}

// newImportWorld opens one client over the given adapters and resolves the user
// scope. The environment is pinned here rather than through newFacadeWorld,
// because a test that cares which adapters answered cannot use the one
// adapter that helper injects.
func newImportWorld(t *testing.T, adapters ...importHost) (*Client, Paths) {
	t.Helper()

	root := t.TempDir()
	user := filepath.Join(root, "user")

	for _, dir := range []string{user, filepath.Join(root, "data"), filepath.Join(root, "project")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	for name, value := range map[string]string{
		"HOME":            user,
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"VERGER_HOME":     "",
		"BEADLE_HOME":     "",
	} {
		t.Setenv(name, value)
	}

	t.Chdir(root)

	injected := make([]host.Host, 0, len(adapters))
	for _, adapter := range adapters {
		injected = append(injected, adapter.build())
	}

	client, err := Open(t.Context(), WithHosts(injected...))
	So(err, ShouldBeNil)

	t.Cleanup(func() { _ = client.Close() })

	paths, err := client.Paths(User, "")
	So(err, ShouldBeNil)

	return client, paths
}

// `verger import` is a batch `adopt`: adopt takes one `host:ref` the user names,
// import takes everything the hosts already have and writes it into the spec
// without the user typing a single ref.
//
// The rule that shapes it: it writes the spec and nothing else. A user who runs
// it expecting a delivery must not get a configuration record instead, which is
// why the CLI says so in `--help` and why nothing in this file delivers
// anything.
func TestImportPlanCollectsWhatHostsInstalledNatively(t *testing.T) {
	Convey("Given two hosts that installed packages natively", t, func() {
		client, paths := newImportWorld(t,
			importHost{id: host.Claude, installed: []host.Installed{
				{Name: "caveman", Version: "1.2.3", Enabled: true},
				{Name: "gws-review", Version: "2.0.0", Enabled: true},
			}},
			importHost{id: host.Codex, installed: []host.Installed{
				{Name: "caveman", Version: "1.2.3", Enabled: true},
				{Name: "extra", Version: "3.1.0", Enabled: true},
			}},
		)

		// One of them is already declared, so it is not a candidate: a second
		// run of import must be a no-op, and a plan that re-proposed what the
		// spec already says would print work it would not do.
		original := "schema = 1\n\n[[package]]\nid = \"caveman\"\n"
		writeFacadeFile(t, paths.SpecPath, original)

		plan, err := client.ImportPlan(t.Context(), ImportOptions{Paths: paths})

		Convey("When the plan is built", func() {
			Convey("Then every package the spec does not declare is a candidate", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 2)

				So(plan.Candidates[0].ID, ShouldEqual, "extra")
				So(plan.Candidates[0].Host, ShouldEqual, "codex")
				So(plan.Candidates[0].Version, ShouldEqual, "3.1.0")

				So(plan.Candidates[1].ID, ShouldEqual, "gws-review")
				So(plan.Candidates[1].Host, ShouldEqual, "claude")
			})

			Convey("And the one already declared is counted, not proposed", func() {
				So(err, ShouldBeNil)
				So(plan.AlreadyInSpec, ShouldEqual, 2)
			})

			Convey("And nothing was written to build the plan", func() {
				after, readErr := os.ReadFile(paths.SpecPath) //nolint:gosec // G304: the test wrote this path
				So(readErr, ShouldBeNil)
				So(string(after), ShouldEqual, original)
			})
		})
	})
}

// The incomplete entry. A host that lists something with no name has told us
// nothing a spec line could carry, and a line with an empty id is a spec no
// command can act on.
func TestImportPlanSkipsWhatItCannotRecord(t *testing.T) {
	Convey("Given a host listing an entry with no name", t, func() {
		client, paths := newImportWorld(t, importHost{id: host.Claude, installed: []host.Installed{
			{Version: "1.0.0", Enabled: true},
			{Name: "caveman", Version: "1.2.3", Enabled: true},
		}})

		plan, err := client.ImportPlan(t.Context(), ImportOptions{Paths: paths})

		Convey("When the plan is built", func() {
			Convey("Then the nameless entry is skipped with a reason and the rest is kept", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 1)
				So(plan.Candidates[0].ID, ShouldEqual, "caveman")

				So(len(plan.Skipped), ShouldEqual, 1)
				So(plan.Skipped[0].Host, ShouldEqual, "claude")
				So(plan.Skipped[0].Reason, ShouldNotBeEmpty)
			})
		})
	})
}

// The partial answer. One host that cannot list must not cost the user the
// hosts that can, and it must not pass for a machine with nothing installed —
// which is why the failure is in the plan and not in an error.
func TestImportPlanSurvivesAHostThatCannotAnswer(t *testing.T) {
	Convey("Given one host whose listing fails", t, func() {
		client, paths := newImportWorld(t,
			importHost{id: host.Claude, installed: []host.Installed{
				{Name: "caveman", Version: "1.2.3", Enabled: true},
			}},
			importHost{id: host.Codex, silent: true},
		)

		plan, err := client.ImportPlan(t.Context(), ImportOptions{Paths: paths})

		Convey("When the plan is built", func() {
			Convey("Then the working host still contributes and the other is named", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 1)
				So(plan.Candidates[0].ID, ShouldEqual, "caveman")

				So(len(plan.Skipped), ShouldEqual, 1)
				So(plan.Skipped[0].Host, ShouldEqual, "codex")
				So(plan.Skipped[0].Reason, ShouldContainSubstring, "plugin list failed")
			})
		})
	})
}

// The second law of the verb, and the one a user's own file depends on: import
// adds lines and preserves every byte it did not write. A hand-written spec
// full of comments is the normal case, not the exotic one.
func TestRecordImportsPreservesTheUsersSpec(t *testing.T) {
	Convey("Given a spec written by hand, with comments", t, func() {
		client, paths := newImportWorld(t)

		original := "# the canon, kept in step with beadle's\n" +
			"schema = 1\n" +
			"\n" +
			"# what to do on a first install\n" +
			"[defaults]\n" +
			"hooks = \"yes\"\n" +
			"\n" +
			"# the review agent, per the incident of 2026-06\n" +
			"[[package]]\n" +
			"id = \"caveman\"\n" +
			"channel = \"stable\" # earns its keep\n"

		writeFacadeFile(t, paths.SpecPath, original)

		plan := ImportPlan{
			Paths:      paths,
			Candidates: []ImportCandidate{{ID: "gws-review", Version: "2.0.0", Host: "claude"}},
		}

		added, err := client.RecordImports(plan)

		Convey("When the imports are recorded", func() {
			Convey("Then the entry is added and every other byte is the user's", func() {
				So(err, ShouldBeNil)
				So(added, ShouldEqual, 1)

				after, readErr := os.ReadFile(paths.SpecPath) //nolint:gosec // G304: the test wrote this path
				So(readErr, ShouldBeNil)

				after2 := string(after)

				So(after2, ShouldContainSubstring, "# the canon, kept in step with beadle's")
				So(after2, ShouldContainSubstring, "# what to do on a first install")
				So(after2, ShouldContainSubstring, "# the review agent, per the incident of 2026-06")
				So(after2, ShouldContainSubstring, `channel = "stable" # earns its keep`)

				So(after2, ShouldContainSubstring, "[[package]]")
				So(after2, ShouldContainSubstring, `id = "gws-review"`)
			})
		})
	})
}

// Idempotence. The second run has nothing to do, and "nothing to do" must mean
// the file is not rewritten — a save that rewrites identical bytes still moves
// the file out from under whatever is reading it.
func TestRecordImportsIsIdempotent(t *testing.T) {
	Convey("Given a plan whose entry is already recorded", t, func() {
		client, paths := newImportWorld(t)

		original := "schema = 1\n\n[[package]]\nid = \"caveman\"\n"
		writeFacadeFile(t, paths.SpecPath, original)

		plan := ImportPlan{
			Paths:      paths,
			Candidates: []ImportCandidate{{ID: "caveman", Version: "1.2.3", Host: "claude"}},
		}

		before, err := os.Stat(paths.SpecPath)
		So(err, ShouldBeNil)

		added, err := client.RecordImports(plan)
		So(err, ShouldBeNil)
		So(added, ShouldEqual, 0)

		Convey("When the same plan is recorded again", func() {
			added, err := client.RecordImports(plan)

			Convey("Then nothing is added, the bytes stand and the file is not replaced", func() {
				So(err, ShouldBeNil)
				So(added, ShouldEqual, 0)

				after, readErr := os.ReadFile(paths.SpecPath) //nolint:gosec // G304: the test wrote this path
				So(readErr, ShouldBeNil)
				So(string(after), ShouldEqual, original)

				// Byte equality is not enough. A writer that saves the same
				// document replaces the file, and anything watching it - a
				// sync daemon, a backup, a `make` rule keyed on the spec -
				// sees a change that did not happen.
				current, statErr := os.Stat(paths.SpecPath)
				So(statErr, ShouldBeNil)
				So(os.SameFile(before, current), ShouldBeTrue)
			})
		})
	})
}

// One id, two hosts. The plan must propose it once: two entries with the same
// id in one spec is a document no command can read, and `AddSpecPackage` would
// have dropped the second silently — the plan is where that has to be visible.
func TestImportPlanProposesADuplicateOnce(t *testing.T) {
	Convey("Given the same package installed on two hosts", t, func() {
		client, paths := newImportWorld(t,
			importHost{id: host.Claude, installed: []host.Installed{
				{Name: "caveman", Version: "1.2.3", Enabled: true},
			}},
			importHost{id: host.Codex, installed: []host.Installed{
				{Name: "caveman", Version: "1.2.3", Enabled: true},
			}},
		)

		plan, err := client.ImportPlan(t.Context(), ImportOptions{Paths: paths})

		Convey("When the plan is built", func() {
			Convey("Then the package is proposed once, by the first host that had it", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 1)
				So(plan.Candidates[0].ID, ShouldEqual, "caveman")
				So(plan.Candidates[0].Host, ShouldEqual, "claude")
			})
		})
	})
}

// A disabled package is still the user's package. Dropping it would make
// `import` lossy on exactly the entries a user most wants written down.
func TestImportPlanKeepsADisabledPackage(t *testing.T) {
	Convey("Given a host reporting one package as disabled", t, func() {
		client, paths := newImportWorld(t, importHost{id: host.Claude, installed: []host.Installed{
			{Name: "caveman", Version: "1.2.3", Enabled: false},
		}})

		plan, err := client.ImportPlan(t.Context(), ImportOptions{Paths: paths})

		Convey("When the plan is built", func() {
			Convey("Then it is a candidate, recorded as disabled", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 1)
				So(plan.Candidates[0].ID, ShouldEqual, "caveman")
				So(plan.Candidates[0].Disabled, ShouldBeTrue)
			})
		})
	})
}

// A host that lists a package with no version. adopt records it and annotates
// it; import does the same rather than dropping what the user already has.
func TestImportPlanKeepsAPackageWithNoVersion(t *testing.T) {
	Convey("Given a host listing a package with no version", t, func() {
		client, paths := newImportWorld(t, importHost{id: host.Claude, installed: []host.Installed{
			{Name: "caveman", Enabled: true},
		}})

		plan, err := client.ImportPlan(t.Context(), ImportOptions{Paths: paths})

		Convey("When the plan is built", func() {
			Convey("Then the package is proposed and the missing version is said out loud", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 1)
				So(plan.Candidates[0].Version, ShouldBeEmpty)
				So(strings.Join(plan.Candidates[0].Notes, " "), ShouldContainSubstring, "no version")
			})
		})
	})
}

// The host filter has to reach the oracles, not only the delivery: a user who
// asks about claude alone is asking what claude has.
func TestImportPlanHonoursTheHostFilter(t *testing.T) {
	Convey("Given two hosts and a filter naming one", t, func() {
		client, paths := newImportWorld(t,
			importHost{id: host.Claude, installed: []host.Installed{
				{Name: "caveman", Version: "1.2.3", Enabled: true},
			}},
			importHost{id: host.Codex, installed: []host.Installed{
				{Name: "extra", Version: "3.1.0", Enabled: true},
			}},
		)

		plan, err := client.ImportPlan(t.Context(), ImportOptions{
			Paths: paths,
			Hosts: HostFilter{Only: []string{string(host.Claude)}},
		})

		Convey("When the plan is built", func() {
			Convey("Then only that host's packages are candidates", func() {
				So(err, ShouldBeNil)
				So(len(plan.Candidates), ShouldEqual, 1)
				So(plan.Candidates[0].ID, ShouldEqual, "caveman")
			})
		})
	})
}
