package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/apply"
	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/lock"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/store"
)

const fixtureRoot = "testdata/claude/acme"

// fakeClaude puts an executable `claude` shim at the front of PATH so the
// resolver finds a binary; the ScriptRunner answers every actual call.
func fakeClaude(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newClaude builds the adapter with a scripted runner and the given options.
func newClaude(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewClaude(all...), runner
}

// response is a successful scripted answer.
func response(stdout string) hostcli.Response {
	return hostcli.Response{Stdout: []byte(stdout)}
}

// claudePackage parses the fixture and adds the rule component the Claude
// manifest format does not carry.
func claudePackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, fixtureRoot, manifest.FormatClaude)
}

// claudeList is the real `claude plugin list --json` answer (Claude Code
// 2.1.283 shape, F7) for the installed plugin ids.
func claudeList(ids ...string) string {
	entries := make([]string, 0, len(ids))

	for _, id := range ids {
		entries = append(entries,
			`{"id":"`+id+`","version":"1.2.3","scope":"user","enabled":true,"installPath":"/tmp/plugins/cache/`+id+`"}`)
	}

	return "[" + strings.Join(entries, ",") + "]"
}

// matchingList is the legacy name-shaped oracle answer, kept for the hosts
// whose real list shape is unverified (Codex OQ-T1.7.1, Gemini OQ-T1.8.1).
func matchingList(name string) string {
	return `[{"name":"` + name + `","marketplace":"plugins","version":"1.2.3","path":"/tmp/plugin"}]`
}

func TestClaudeDetect(t *testing.T) {
	Convey("Given a home without a Claude config dir", t, func() {
		home := t.TempDir()

		Convey("When no binary and no config dir exist", func() {
			t.Setenv("PATH", t.TempDir())
			h, _ := newClaude(t, home, nil)

			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When the config dir exists", func() {
			t.Setenv("PATH", t.TempDir())

			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			h, _ := newClaude(t, home, nil)

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When only the binary resolves", func() {
			fakeClaude(t)
			h, _ := newClaude(t, home, nil)

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When a CLAUDE_CONFIG_DIR profile exists", func() {
			t.Setenv("PATH", t.TempDir())

			profile := filepath.Join(t.TempDir(), "claude-work")

			if err := os.MkdirAll(profile, 0o700); err != nil {
				t.Fatalf("mkdir profile: %v", err)
			}

			t.Setenv("CLAUDE_CONFIG_DIR", profile)
			h, _ := newClaude(t, home, nil)

			Convey("Then the profile home detects the host", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

func TestClaudeKnownAndAliases(t *testing.T) {
	Convey("Given the host vocabulary", t, func() {
		all := host.All()

		Convey("Then every spec id is known and All is complete and ordered", func() {
			So(all, ShouldHaveLength, 9)
			So(all, ShouldResemble, []host.ID{
				host.Claude, host.Codex, host.Gemini, host.Agy, host.Cursor,
				host.OpenCode, host.Kilo, host.Pi, host.DSH,
			})

			for _, id := range all {
				So(host.Known(id), ShouldBeTrue)
			}

			So(host.Known("unknown"), ShouldBeFalse)
			So(host.Known(""), ShouldBeFalse)
		})

		Convey("Then the strategy aliases are the lock strategies", func() {
			So(host.Native, ShouldEqual, lock.StrategyNative)
			So(host.Synth, ShouldEqual, lock.StrategySynth)
			So(host.Loose, ShouldEqual, lock.StrategyLoose)
			So(host.Silenced, ShouldEqual, lock.StrategySilenced)
		})
	})
}

func TestClaudeNativeDeliver(t *testing.T) {
	Convey("Given a native package and a scripted claude", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		ref := "https://github.com/acme/plugins.git"

		script := map[string]hostcli.Response{
			"claude plugin marketplace add " + ref:  response(""),
			"claude plugin install caveman@plugins": response(""),
			"claude plugin list --json":             response(claudeList("caveman@plugins")),
		}

		h, runner := newClaude(t, home, script)

		pkg := claudePackage(t)
		pkg.Marketplace = ref

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the exact argv runs in order and the oracle verifies", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"claude plugin marketplace add " + ref,
					"claude plugin install caveman@plugins",
					"claude plugin list --json",
				})
				So(res.Strategy, ShouldEqual, host.Native)
				So(res.Observed.Verified, ShouldBeTrue)
				So(res.Observed.Listed, ShouldHaveLength, 1)
			})

			Convey("Then the RMA carries both host-install inverses", func() {
				So(res.RMA, ShouldHaveLength, 2)

				So(res.RMA[0].Kind, ShouldEqual, receipt.OpHostInstall)
				So(res.RMA[0].Command, ShouldResemble, []string{"plugin", "marketplace", "rm", "plugins"})

				So(res.RMA[1].Kind, ShouldEqual, receipt.OpHostInstall)
				So(res.RMA[1].Command, ShouldResemble, []string{"plugin", "uninstall", "caveman@plugins"})
			})
		})
	})
}

// claudeSynthWorld is one Claude home served by the stateful fake CLI, with a
// store for synth packages and the trash.
func claudeSynthWorld(t *testing.T) (host.Host, *claudeCLI, *store.Store, string) {
	t.Helper()

	fakeClaude(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	home := t.TempDir()
	st := openStore(t)
	cli := newClaudeCLI(filepath.Join(home, ".claude"))

	return host.NewClaude(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash())), cli, st, home
}

// ownerRoot is the owner marketplace root of one owner in the store.
func ownerRoot(st *store.Store, owner string) string {
	return filepath.Join(st.Root(), "synth", owner)
}

// ownerDoc reads the owner marketplace document.
func ownerDoc(t *testing.T, st *store.Store, owner string) string {
	t.Helper()

	return readTestFile(t, filepath.Join(ownerRoot(st, owner), ".claude-plugin", "marketplace.json"))
}

// TestClaudeSynthDeliver pins decision F3 on the Claude synth rung: the owner
// is the marketplace (<store>/synth/<owner>), the package is plugin <name> in
// it, the reference is name@owner; a known marketplace is updated, not
// re-added, and an installed plugin is updated, not re-installed.
func TestClaudeSynthDeliver(t *testing.T) {
	Convey("Given a synth package acme/caveman 1.0.0 in the store", t, func() {
		h, cli, st, _ := claudeSynthWorld(t)
		pkg := synthPackage(t, st, "acme/caveman", "1.0.0")
		root := ownerRoot(st, "acme")

		Convey("When it is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})
			So(err, ShouldBeNil)

			Convey("Then the owner marketplace is registered and the plugin installed as caveman@acme", func() {
				So(cli.Calls(), ShouldResemble, []string{
					"claude plugin marketplace list --json",
					"claude plugin marketplace add " + root,
					"claude plugin list --json",
					"claude plugin install caveman@acme",
					"claude plugin list --json",
				})

				installed, ok := cli.Installed("caveman@acme")
				So(ok, ShouldBeTrue)
				So(installed, ShouldEqual, "1.0.0")
				So(res.Observed.Verified, ShouldBeTrue)
			})

			Convey("Then the owner document lists the package by its bare name", func() {
				So(ownerDoc(t, st, "acme"), ShouldEqualJSON,
					`{"name":"acme","owner":{"name":"acme"},"plugins":[{"name":"caveman","source":"./caveman/1.0.0"}]}`)
			})

			Convey("Then the RMA removes the install and the marketplace", func() {
				So(res.RMA, ShouldResemble, []receipt.Op{
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "acme"}},
					{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@acme"}},
				})
			})

			Convey("Then a second package of the owner joins the same marketplace", func() {
				other := synthPackage(t, st, "acme/other", "2.0.0")

				_, otherErr := h.Deliver(t.Context(), "", host.Delivery{Package: other, Strategy: host.Synth})
				So(otherErr, ShouldBeNil)

				So(ownerDoc(t, st, "acme"), ShouldEqualJSON, `{"name":"acme","owner":{"name":"acme"},"plugins":[
  {"name":"caveman","source":"./caveman/1.0.0"},
  {"name":"other","source":"./other/2.0.0"}]}`)

				calls := cli.Calls()
				So(calls, ShouldContain, "claude plugin marketplace update acme")
				So(calls, ShouldContain, "claude plugin install other@acme")
			})

			Convey("Then a new version rewrites the entry, updates the marketplace and then the plugin", func() {
				next := synthPackage(t, st, "acme/caveman", "1.1.0")

				nextRes, nextErr := h.Deliver(t.Context(), "", host.Delivery{Package: next, Strategy: host.Synth})
				So(nextErr, ShouldBeNil)
				So(nextRes.RMA, ShouldResemble, res.RMA)

				So(ownerDoc(t, st, "acme"), ShouldEqualJSON,
					`{"name":"acme","owner":{"name":"acme"},"plugins":[{"name":"caveman","source":"./caveman/1.1.0"}]}`)

				calls := cli.Calls()
				So(calls[5:], ShouldResemble, []string{
					"claude plugin marketplace list --json",
					"claude plugin marketplace update acme",
					"claude plugin list --json",
					"claude plugin update caveman@acme",
					"claude plugin list --json",
				})

				installed, _ := cli.Installed("caveman@acme")
				So(installed, ShouldEqual, "1.1.0")
			})
		})

		Convey("When it is only planned", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth, DryRun: true})

			Convey("Then only the read-only marketplace probe runs and nothing is written", func() {
				So(err, ShouldBeNil)
				So(cli.Calls(), ShouldResemble, []string{"claude plugin marketplace list --json"})
				So(res.RMA, ShouldHaveLength, 2)
				So(fileExists(filepath.Join(root, ".claude-plugin", "marketplace.json")), ShouldBeFalse)
			})
		})
	})
}

// TestClaudeSynthMarketplaceCollision pins the F3 collision rule: a foreign
// marketplace already named after the owner is never touched; verger's owner
// marketplace becomes <owner>-verger, and the RMA names it.
func TestClaudeSynthMarketplaceCollision(t *testing.T) {
	Convey("Given a user's own marketplace named acme", t, func() {
		h, cli, st, _ := claudeSynthWorld(t)
		foreign := t.TempDir()
		cli.marketplaces["acme"] = foreign

		pkg := synthPackage(t, st, "acme/caveman", "1.0.0")

		Convey("When the synth package is delivered", func() {
			res, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then it lands as caveman@acme-verger and the user's marketplace is untouched", func() {
				So(err, ShouldBeNil)
				So(ownerDoc(t, st, "acme"), ShouldContainSubstring, `"name": "acme-verger"`)

				_, installed := cli.Installed("caveman@acme-verger")
				So(installed, ShouldBeTrue)

				dir, _ := cli.Registered("acme")
				So(dir, ShouldEqual, foreign)
				So(res.RMA[0].Command, ShouldResemble, []string{"plugin", "marketplace", "rm", "acme-verger"})
			})
		})

		Convey("When acme-verger is foreign too", func() {
			cli.marketplaces["acme-verger"] = t.TempDir()

			_, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then the cell is hands-off before any write", func() {
				_, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(cli.Calls(), ShouldResemble, []string{"claude plugin marketplace list --json"})
				So(fileExists(filepath.Join(ownerRoot(st, "acme"), ".claude-plugin", "marketplace.json")), ShouldBeFalse)
			})
		})
	})
}

// TestClaudeSynthPlanRefusals pins the plan-time refusals of the synth rung:
// a synth dir outside the store layout, and a plugin name another package of
// the owner already holds (the name projection is not injective).
func TestClaudeSynthPlanRefusals(t *testing.T) {
	Convey("Given a synth dir outside <store>/synth/<owner>/<name>/<version>", t, func() {
		h, cli, _, _ := claudeSynthWorld(t)

		pkg := host.Package{ID: "acme/caveman", Version: "1.0.0", SynthDir: t.TempDir(), Marketplace: "caveman@acme"}

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: pkg, Strategy: host.Synth})

		Convey("When it is delivered", func() {
			Convey("Then the plan refuses it and nothing runs", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "plan")
				So(cli.Calls(), ShouldBeEmpty)
			})
		})
	})

	Convey("Given the owner document already lists b-c for another package", t, func() {
		h, cli, st, _ := claudeSynthWorld(t)

		first := synthPackage(t, st, "acme/b/c", "1.0.0")
		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: first, Strategy: host.Synth})
		So(err, ShouldBeNil)

		second := synthPackage(t, st, "acme/b-c", "1.0.0")
		before := len(cli.Calls())

		_, secondErr := h.Deliver(t.Context(), "", host.Delivery{Package: second, Strategy: host.Synth})

		Convey("When a package projecting onto the same plugin name is delivered", func() {
			Convey("Then the plan refuses it before any host call and the first entry is kept", func() {
				typed, ok := errors.AsType[*host.DeliveryError](secondErr)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "plan")
				So(ownerDoc(t, st, "acme"), ShouldContainSubstring, `"./b+2Fc/1.0.0"`)
				So(cli.Calls(), ShouldHaveLength, before)
			})
		})
	})
}

// TestClaudeSynthRefcount pins §4.8/F3 removal: uninstalling a package keeps
// the owner marketplace while another of its plugins is installed, removes it
// with the last one, and moves the plugin cache Claude leaves behind to the
// trash.
func TestClaudeSynthRefcount(t *testing.T) {
	Convey("Given two packages of one owner installed from its marketplace", t, func() {
		h, cli, st, home := claudeSynthWorld(t)

		first := synthPackage(t, st, "acme/caveman", "1.0.0")
		second := synthPackage(t, st, "acme/other", "2.0.0")

		firstRes, err := h.Deliver(t.Context(), "", host.Delivery{Package: first, Strategy: host.Synth})
		So(err, ShouldBeNil)

		secondRes, err := h.Deliver(t.Context(), "", host.Delivery{Package: second, Strategy: host.Synth})
		So(err, ShouldBeNil)

		cache := filepath.Join(home, ".claude", "plugins", "cache", "acme")

		uninstall := func(pkg host.Package, rma []receipt.Op) host.Result {
			res, uninstallErr := h.Uninstall(t.Context(), "", receipt.Receipt{
				Package: pkg.ID, Host: "claude", Scope: receipt.ScopeUser, Strategy: string(host.Synth), RMA: rma,
			})
			So(uninstallErr, ShouldBeNil)

			return res
		}

		Convey("When the first package is uninstalled", func() {
			res := uninstall(first, firstRes.RMA)

			Convey("Then the marketplace stays for the other plugin and the cache goes to the trash", func() {
				_, registered := cli.Registered("acme")
				So(registered, ShouldBeTrue)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "still serves")

				_, installed := cli.Installed("caveman@acme")
				So(installed, ShouldBeFalse)
				So(fileExists(filepath.Join(cache, "caveman")), ShouldBeFalse)
				So(fileExists(filepath.Join(cache, "other")), ShouldBeTrue)
				So(trashHolds(t, st, filepath.Join(cache, "caveman")), ShouldBeTrue)
			})

			Convey("Then the last package takes the marketplace and its cache dir with it", func() {
				uninstall(second, secondRes.RMA)

				_, registered := cli.Registered("acme")
				So(registered, ShouldBeFalse)
				So(fileExists(cache), ShouldBeFalse)
			})
		})
	})
}

// TestClaudeSynthRollback pins "a failed delivery leaves no registered
// marketplace": apply rolls the failure back with the dry-run RMA, the
// uninstall of a plugin that never landed is tolerated, and the refcount
// then removes the marketplace — unless another plugin still uses it.
func TestClaudeSynthRollback(t *testing.T) {
	Convey("Given a synth install the host refuses", t, func() {
		h, cli, st, _ := claudeSynthWorld(t)
		cli.fail["plugin install caveman@acme"] = hostcli.Response{Code: 1, Stderr: "✘ Failed to install plugin \"caveman@acme\": boom"}

		deps := applyWorld(t, st, ownerMap{})
		deps.Hosts[host.Claude] = h

		pkg := synthPackage(t, st, "acme/caveman", "1.0.0")

		run := func(p host.Package) apply.CellResult {
			report, err := apply.Run(t.Context(), deps, apply.Plan{Actions: []apply.Action{{
				Kind: apply.ActionInstall, Host: host.Claude, Delivery: host.Delivery{Package: p, Strategy: host.Synth},
			}}}, apply.Options{})
			So(err, ShouldBeNil)

			return report.Cells[0]
		}

		Convey("When apply installs it alone", func() {
			cell := run(pkg)

			Convey("Then the cell fails and the owner marketplace is not left registered", func() {
				So(cell.Status, ShouldEqual, apply.StatusFailed)

				_, registered := cli.Registered("acme")
				So(registered, ShouldBeFalse)
				So(strings.Join(cell.Notes, "\n"), ShouldNotContainSubstring, "rollback:")
			})
		})

		Convey("When another plugin of the owner is already installed", func() {
			So(run(synthPackage(t, st, "acme/other", "2.0.0")).Status, ShouldEqual, apply.StatusCurrent)

			cell := run(pkg)

			Convey("Then the rollback keeps the marketplace the other plugin needs", func() {
				So(cell.Status, ShouldEqual, apply.StatusFailed)

				_, registered := cli.Registered("acme")
				So(registered, ShouldBeTrue)

				_, installed := cli.Installed("other@acme")
				So(installed, ShouldBeTrue)
			})
		})
	})
}

// trashHolds reports whether the store trash holds one original path.
func trashHolds(t *testing.T, st *store.Store, original string) bool {
	t.Helper()

	entries, err := st.Trash().List()
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}

	for _, entry := range entries {
		if entry.Original == original {
			return true
		}
	}

	return false
}

func TestClaudeNativeRefusals(t *testing.T) {
	Convey("Given a native package without a marketplace", t, func() {
		home := t.TempDir()
		h, runner := newClaude(t, home, nil)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: claudePackage(t), Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then it is a *NotSupportedError and nothing runs", func() {
				_, ok := errors.AsType[*host.NotSupportedError](err)
				So(ok, ShouldBeTrue)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})

	Convey("Given a silenced delivery", t, func() {
		home := t.TempDir()
		h, _ := newClaude(t, home, nil)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: claudePackage(t), Strategy: host.Silenced})

		Convey("When it is delivered", func() {
			Convey("Then it is an *UnsupportedStrategyError", func() {
				typed, ok := errors.AsType[*host.UnsupportedStrategyError](err)
				So(ok, ShouldBeTrue)
				So(typed.Host, ShouldEqual, host.Claude)
				So(typed.Strategy, ShouldEqual, host.Silenced)
			})
		})
	})
}

func TestClaudeNativeVerifyFailure(t *testing.T) {
	Convey("Given a native install the oracle does not list", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		script := map[string]hostcli.Response{
			"claude plugin marketplace add acme/plugins": response(""),
			"claude plugin install caveman@plugins":      response(""),
			"claude plugin list --json":                  response(claudeList("caveman@other-market", "other@plugins")),
		}

		h, runner := newClaude(t, home, script)

		pkg := claudePackage(t)
		pkg.Marketplace = "acme/plugins"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then a *DeliveryError names the verify step and no fallback runs", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "verify")
				So(typed.Host, ShouldEqual, "claude")
				So(callKeys(runner), ShouldHaveLength, 3)
			})
		})
	})
}

func TestClaudePolicyGate(t *testing.T) {
	Convey("Given claude settings blocking the marketplace", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		ref := "https://github.com/acme/plugins.git"

		writeSettings(t, home, `{
  "blockedMarketplaces": ["`+ref+`"]
}`)

		h, runner := newClaude(t, home, nil)

		pkg := claudePackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then no marketplace argv runs and the policy rule is named", func() {
				typed, ok := errors.AsType[*host.PolicyError](err)
				So(ok, ShouldBeTrue)
				So(typed.Rule, ShouldEqual, "blockedMarketplaces")
				So(typed.Ref, ShouldEqual, ref)
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})

	Convey("Given strictKnownMarketplaces not listing the ref", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		writeSettings(t, home, `{
  "strictKnownMarketplaces": ["https://github.com/other/marketplace.git"]
}`)

		h, runner := newClaude(t, home, nil)

		pkg := claudePackage(t)
		pkg.Marketplace = "https://github.com/acme/plugins.git"

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the strict allowlist blocks it", func() {
				typed, ok := errors.AsType[*host.PolicyError](err)
				So(ok, ShouldBeTrue)
				So(typed.Rule, ShouldEqual, "strictKnownMarketplaces")
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})

	Convey("Given strictKnownMarketplaces listing the ref", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		ref := "https://github.com/acme/plugins.git"

		writeSettings(t, home, `{
  "strictKnownMarketplaces": ["`+ref+`"]
}`)

		script := map[string]hostcli.Response{
			"claude plugin marketplace add " + ref:  response(""),
			"claude plugin install caveman@plugins": response(""),
			"claude plugin list --json":             response(claudeList("caveman@plugins")),
		}

		h, runner := newClaude(t, home, script)

		pkg := claudePackage(t)
		pkg.Marketplace = ref

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When it is delivered", func() {
			Convey("Then the allowed ref installs", func() {
				So(err, ShouldBeNil)
				So(runner.Calls(), ShouldHaveLength, 3)
			})
		})
	})
}

func TestClaudeNativeDryRun(t *testing.T) {
	Convey("Given a native dry run", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		before := snapshotHome(t, home)

		h, runner := newClaude(t, home, nil)

		pkg := claudePackage(t)
		pkg.Marketplace = "https://github.com/acme/plugins.git"

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native, DryRun: true})

		Convey("When it is delivered", func() {
			Convey("Then no runner call or write happens and the plan is returned", func() {
				So(err, ShouldBeNil)
				So(runner.Calls(), ShouldBeEmpty)
				So(snapshotHome(t, home), ShouldResemble, before)
				So(res.RMA, ShouldHaveLength, 2)
				So(res.Notes, ShouldContain, "dry-run")
			})
		})
	})
}

func TestClaudeUninstall(t *testing.T) {
	Convey("Given a receipt with host-install RMA ops", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		script := map[string]hostcli.Response{
			"claude plugin uninstall caveman@plugins": response(""),
			"claude plugin list --json":               response("[]"),
			"claude plugin marketplace rm plugins":    response(""),
		}

		h, runner := newClaude(t, home, script)

		r := receipt.Receipt{
			Package:  "acme/caveman",
			Host:     "claude",
			Scope:    receipt.ScopeUser,
			Strategy: string(host.Native),
			RMA: []receipt.Op{
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "marketplace", "rm", "plugins"}},
				{Kind: receipt.OpHostInstall, Command: []string{"plugin", "uninstall", "caveman@plugins"}},
				{Kind: receipt.OpWriteFile, Path: filepath.Join(home, "file"), Digest: "aa"},
			},
		}

		res, err := h.Uninstall(t.Context(), home, r)

		Convey("When it is uninstalled", func() {
			Convey("Then host ops run in reverse order, the marketplace only once the oracle shows it unused", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldResemble, []string{
					"claude plugin uninstall caveman@plugins",
					"claude plugin list --json",
					"claude plugin list --json",
					"claude plugin marketplace rm plugins",
				})
				So(res.Strategy, ShouldEqual, host.Native)
			})
		})
	})
}

// TestClaudeOracleListRealShape pins [F7]: the real `claude plugin list
// --json` of Claude Code 2.1.283 (testdata/claude/plugin-list-2.1.283.json,
// captured live) carries `id` = "<name>@<marketplace>" and `installPath`, no
// `name`/`marketplace`/`path`; `enabled` and `scope` come through.
func TestClaudeOracleListRealShape(t *testing.T) {
	Convey("Given the real plugin list output of Claude Code 2.1.283", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		output, err := os.ReadFile(filepath.Join("testdata", "claude", "plugin-list-2.1.283.json"))
		So(err, ShouldBeNil)

		h, _ := newClaude(t, home, map[string]hostcli.Response{"claude plugin list --json": {Stdout: output}})

		Convey("When the oracle lists it", func() {
			listed, listErr := h.Oracle().List(t.Context())

			Convey("Then every entry is split at the last @ and keeps its path, scope and state", func() {
				So(listErr, ShouldBeNil)
				So(listed, ShouldResemble, []host.Installed{
					{
						Name: "foo", Marketplace: "acme", Version: "1.1.0", Scope: "user", Enabled: true,
						Path: "/home/u/.claude/plugins/cache/acme/foo/1.1.0",
					},
					{
						Name: "skills--find-skills", Marketplace: "vercel-labs", Version: "1.0.0", Scope: "user", Enabled: false,
						Path: "/home/u/.claude/plugins/cache/vercel-labs/skills--find-skills/1.0.0",
					},
				})
			})
		})
	})

	Convey("Given entries of other shapes", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		cases := []struct {
			label  string
			output string
			want   []host.Installed
		}{
			{
				"a legacy name/marketplace/path entry without enabled",
				`[{"name":"a","marketplace":"m","path":"/p","version":"1"}]`,
				[]host.Installed{{Name: "a", Marketplace: "m", Path: "/p", Version: "1", Enabled: true}},
			},
			{
				"an id without a marketplace",
				`[{"id":"@bare"},{"id":"plain"}]`,
				nil,
			},
			{
				"a plugin entry whose own fields nest another name",
				`[{"id":"a@m","extra":{"name":"not-a-plugin"}}]`,
				[]host.Installed{{Name: "a", Marketplace: "m", Enabled: true}},
			},
		}

		for _, item := range cases {
			Convey("When the oracle parses "+item.label, func() {
				h, _ := newClaude(t, home, map[string]hostcli.Response{"claude plugin list --json": response(item.output)})

				listed, err := h.Oracle().List(t.Context())

				Convey("Then only plugin entries are taken, whole", func() {
					if item.want == nil {
						_, ok := errors.AsType[*host.OracleError](err)
						So(ok, ShouldBeTrue)

						return
					}

					So(err, ShouldBeNil)
					So(listed, ShouldResemble, item.want)
				})
			})
		}
	})
}

func TestClaudeOracleList(t *testing.T) {
	Convey("Given a scripted claude plugin list", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		cases := []struct {
			name   string
			output string
			want   int
		}{
			{"flat array", `[{"name":"a","version":"1"},{"name":"b"}]`, 2},
			{"wrapped object", `{"plugins":[{"name":"a","marketplace":"m"}]}`, 1},
			{"deep nesting", `{"data":{"installed":{"items":[{"name":"a","path":"/p"}]}}}`, 1},
			{"noise fields", `[{"foo":1,"name":"a","extra":{"x":true}},{"nope":2}]`, 1},
			{"empty array", `[]`, 0},
			{"empty object", `{}`, 0},
		}

		for _, item := range cases {
			Convey("When the oracle parses "+item.name, func() {
				h, _ := newClaude(t, home, map[string]hostcli.Response{
					"claude plugin list --json": response(item.output),
				})

				listed, err := h.Oracle().List(t.Context())

				Convey("Then the entries are extracted tolerantly", func() {
					So(err, ShouldBeNil)
					So(listed, ShouldHaveLength, item.want)
				})
			})
		}

		Convey("When the output is garbage", func() {
			h, _ := newClaude(t, home, map[string]hostcli.Response{
				"claude plugin list --json": response("not json at all"),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then it is an *OracleError, never a silent empty success", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Host, ShouldEqual, "claude")
				So(typed.Output, ShouldContainSubstring, "not json")
			})
		})

		Convey("When the output is a non-container scalar", func() {
			h, _ := newClaude(t, home, map[string]hostcli.Response{
				"claude plugin list --json": response(`"ok"`),
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then it is an *OracleError", func() {
				_, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
			})
		})

		Convey("When the CLI exits non-zero", func() {
			h, _ := newClaude(t, home, map[string]hostcli.Response{
				"claude plugin list --json": {Code: 3, Stderr: "boom"},
			})

			_, err := h.Oracle().List(t.Context())

			Convey("Then the *ExitError passes through unchanged", func() {
				typed, ok := errors.AsType[*hostcli.ExitError](err)
				So(ok, ShouldBeTrue)
				So(typed.Code, ShouldEqual, 3)
			})
		})

		Convey("When the context is canceled", func() {
			h, _ := newClaude(t, home, nil)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := h.Oracle().List(ctx)

			Convey("Then the context error surfaces", func() {
				So(errors.Is(err, context.Canceled), ShouldBeTrue)
			})
		})
	})
}

func TestClaudeOracleValidate(t *testing.T) {
	Convey("Given a scripted plugin validate", t, func() {
		fakeClaude(t)
		home := t.TempDir()

		Convey("When validation succeeds with output", func() {
			h, _ := newClaude(t, home, map[string]hostcli.Response{
				"claude plugin validate /tmp/synth": response("warning one\nwarning two\n\n"),
			})

			warnings, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then every output line is a warning", func() {
				So(err, ShouldBeNil)
				So(warnings, ShouldResemble, []string{"warning one", "warning two"})
			})
		})

		Convey("When validation fails", func() {
			h, _ := newClaude(t, home, map[string]hostcli.Response{
				"claude plugin validate /tmp/synth": {Code: 1, Stderr: "hooks.json invalid"},
			})

			_, err := h.Oracle().Validate(t.Context(), "/tmp/synth")

			Convey("Then it is an *OracleError carrying the output", func() {
				typed, ok := errors.AsType[*host.OracleError](err)
				So(ok, ShouldBeTrue)
				So(typed.Output, ShouldContainSubstring, "hooks.json invalid")
			})
		})
	})
}

// callKeys renders the ScriptRunner calls as `<name> <joined args>` keys.
func callKeys(runner *hostcli.ScriptRunner) []string {
	calls := runner.Calls()

	out := make([]string, 0, len(calls))

	for _, call := range calls {
		var key strings.Builder

		key.WriteString(call.Binary)

		for _, arg := range call.Args {
			key.WriteString(" ")
			key.WriteString(arg)
		}

		out = append(out, key.String())
	}

	return out
}

// writeSettings writes <home>/.claude/settings.json.
func writeSettings(t *testing.T, home, content string) {
	t.Helper()

	dir := filepath.Join(home, ".claude")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// digestFile digests one fixture file.
func digestFile(t *testing.T, path string) digest.Hash {
	t.Helper()

	sum, err := digest.File(path)
	if err != nil {
		t.Fatalf("digest %s: %v", path, err)
	}

	return sum
}
