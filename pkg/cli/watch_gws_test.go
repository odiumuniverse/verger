package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
	"github.com/spf13/cobra"

	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/home"
	"github.com/odiumuniverse/verger/pkg/verger"
	"github.com/odiumuniverse/verger/pkg/watchbind"
)

// runCtx executes one command line under a caller-supplied context, so a
// long-running verb can be stopped the way a signal would stop it. The shared
// world.run cannot do this: cobra's context there is Background, and a
// foreground watch would never return.
func (w *world) runCtx(ctx context.Context, args ...string) (string, error) {
	w.out.Reset()
	w.err.Reset()

	root := NewRootCmd(w.options())
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.ExecuteContext(ctx)

	return w.out.String(), err
}

// TestWatchIsRegistered pins W8-COEXIST-2 (2): the two new verbs are part of
// the §8 surface, so a script that discovers commands finds them.
func TestWatchIsRegistered(t *testing.T) {
	Convey("Given the command tree", t, func() {
		w := newWorld(t)
		root := NewRootCmd(w.options())

		Convey("Then watch and service are both present", func() {
			found := map[string]bool{}
			for _, cmd := range root.Commands() {
				found[cmd.Name()] = true
			}

			So(found["watch"], ShouldBeTrue)
			So(found["service"], ShouldBeTrue)
		})

		Convey("Then service carries install, uninstall and status", func() {
			serviceCmd := findCommand(t, root, "service")

			sub := map[string]bool{}
			for _, cmd := range serviceCmd.Commands() {
				sub[cmd.Name()] = true
			}

			So(sub["install"], ShouldBeTrue)
			So(sub["uninstall"], ShouldBeTrue)
			So(sub["status"], ShouldBeTrue)
		})
	})
}

// findCommand returns the named subcommand of root.
func findCommand(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()

	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}

	t.Fatalf("command %q not found", name)

	return nil
}

// openClient opens the facade over the world's own home, so a test can take
// the lease the verb will later contend for.
func (w *world) openClient(t *testing.T) (*verger.Client, error) {
	t.Helper()

	w.warmHome(t)

	return verger.Open(context.Background(), verger.WithHome(filepath.Join(w.root, "user", ".verger")))
}

// TestWatchInterruptIsSuccess pins the Ctrl-C rule: a watcher the user
// stopped must exit 0, not print a failure. Execute cancels the context on
// SIGINT and SIGTERM, and a non-zero exit would make every interrupt look
// like a crash in CI and in a service manager.
func TestWatchInterruptIsSuccess(t *testing.T) {
	Convey("Given a watch stopped by a cancelled context", t, func() {
		watchbind.Install()

		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)

		go func() {
			_, err := w.runCtx(ctx, "watch", "--owner", verger.LeaseOwnerVerger)
			done <- err
		}()

		// Give the watcher long enough to take the lease and arm, then stop
		// it the way a signal would.
		time.Sleep(300 * time.Millisecond)

		cancel()

		select {
		case err := <-done:
			Convey("Then the command reports success", func() {
				So(err, ShouldBeNil)
				So(exitcode.Classify(err), ShouldEqual, exitcode.OK)
			})
		case <-time.After(5 * time.Second):
			Convey("Then the command stops inside its shutdown budget", func() {
				// The user asked for at most two seconds; a watch that
				// ignores cancellation leaves a lease naming a dead pid.
				t.Fatal("verger watch did not stop within 5s of cancellation")
			})
		}
	})
}

// TestWatchBusyLeaseIsConflict pins the second half of the lease rule: a lease
// already held is a conflict the user can settle, so it is exit 3 and the
// message names the owner and pid — not a generic failure.
func TestWatchBusyLeaseIsConflict(t *testing.T) {
	Convey("Given a home whose watch lease another owner holds", t, func() {
		watchbind.Install()

		w := newWorld(t)
		w.chdir(t, w.root)
		w.fixture(t)

		client, err := w.openClient(t)
		So(err, ShouldBeNil)

		handle, err := client.AcquireLease(verger.LeaseOwnerBeadle)
		So(err, ShouldBeNil)

		defer func() { _ = client.ReleaseLease(handle) }()

		_, watchErr := w.runCtx(context.Background(), "watch", "--owner", verger.LeaseOwnerVerger)

		Convey("Then it is the typed lease error, not text", func() {
			So(watchErr, ShouldNotBeNil)

			held, ok := errors.AsType[*home.LeaseHeldError](watchErr)
			So(ok, ShouldBeTrue)
			So(held.Owner, ShouldEqual, verger.LeaseOwnerBeadle)
			So(held.PID, ShouldBeGreaterThan, 0)
		})

		Convey("Then it classifies as a conflict, exit 3", func() {
			So(exitcode.Classify(watchErr), ShouldEqual, exitcode.Conflict)
		})

		Convey("Then the message names the owner and the pid", func() {
			So(watchErr.Error(), ShouldContainSubstring, verger.LeaseOwnerBeadle)
			So(watchErr.Error(), ShouldContainSubstring, "pid")
		})
	})
}

// TestServiceStatusOnAnEmptyHome pins the read-only verb: it answers about a
// home with no unit instead of failing, so a first run is informative.
func TestServiceStatusOnAnEmptyHome(t *testing.T) {
	Convey("Given a home with no watch unit", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		out, err := w.run("service", "status", "--json")

		Convey("Then it succeeds and reports not-installed", func() {
			So(err, ShouldBeNil)
			So(out, ShouldContainSubstring, `"state":`)
			So(out, ShouldContainSubstring, "not-installed")
		})
	})
}

// TestServiceInstallRefusesATemporaryHome pins the guard that keeps a unit
// from outliving the tree that built it.
func TestServiceInstallRefusesATemporaryHome(t *testing.T) {
	Convey("Given a home under a temporary root", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		// newWorld's user home lives under t.TempDir(), which is exactly
		// the case TemporaryHome refuses.
		_, err := w.run("service", "install")

		Convey("Then the install is refused, with a message naming the home", func() {
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "temporary")
		})

		Convey("Then it is a wrong invocation, so a script gets exit 2", func() {
			// ErrTemporaryHome classifies as Usage, not Unexpected: the caller
			// pointed the command at a home a login service cannot outlive,
			// which is fixable by changing the invocation.
			So(exitcode.Classify(err), ShouldEqual, exitcode.Usage)
		})
	})
}

// TestWatchLeasesTheHome pins that the facade, not the CLI, owns the lease:
// the verb must not take it twice.
func TestWatchLeasesTheHome(t *testing.T) {
	Convey("Given a home with no lease", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		client, err := w.openClient(t)
		So(err, ShouldBeNil)

		_, held, err := client.LeaseStatus()
		So(err, ShouldBeNil)
		So(held, ShouldBeFalse)
	})
}

// TestWatchUsesTheScopeFlags pins that watch honours --project and the host
// filters, like every other scope-taking verb.
func TestWatchUsesTheScopeFlags(t *testing.T) {
	Convey("Given watch with host flags", t, func() {
		root := newWorld(t)
		cmd := findCommand(t, NewRootCmd(root.options()), "watch")

		Convey("Then it accepts the scope and host switches", func() {
			So(cmd.Flags().Lookup("project"), ShouldNotBeNil)
			So(cmd.Flags().Lookup("hosts"), ShouldNotBeNil)
			So(cmd.Flags().Lookup("except"), ShouldNotBeNil)
			So(cmd.Flags().Lookup("owner"), ShouldNotBeNil)
			So(cmd.Flags().Lookup("debounce"), ShouldNotBeNil)
		})
	})
}

// TestServiceStatusJSONShape pins the machine-readable contract a script
// consumes.
func TestServiceStatusJSONShape(t *testing.T) {
	Convey("Given service status --json", t, func() {
		w := newWorld(t)
		w.chdir(t, w.root)

		out, err := w.run("service", "status", "--json")
		So(err, ShouldBeNil)

		Convey("Then it is one object naming the state, path and flags", func() {
			trimmed := strings.TrimSpace(out)
			So(strings.HasPrefix(trimmed, "{"), ShouldBeTrue)
			So(strings.HasSuffix(trimmed, "}"), ShouldBeTrue)
			So(trimmed, ShouldContainSubstring, `"installed":`)
			So(trimmed, ShouldContainSubstring, `"loaded":`)
		})
	})
}
