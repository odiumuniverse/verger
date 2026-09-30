package host_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
)

// policySourceRef is the source ref of the policy fixtures.
const policySourceRef = "https://github.com/acme/plugins.git"

// policyHome writes a Claude settings.json carrying one policy document.
func policyHome(t *testing.T, doc string) string {
	t.Helper()

	home := t.TempDir()
	writeSettings(t, home, doc)

	return home
}

// policyPackage builds the Claude fixture without MCP CLI calls.
func policyPackage(t *testing.T) host.Package {
	t.Helper()

	return withoutMCP(claudePackage(t))
}

// policyReadFile reads one file the test itself wrote.
func policyReadFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp home
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// TestPolicyBlockedSource pins DESIGN §1.3/§4.8: a source ref listed by
// blockedMarketplaces, or missing from a non-empty strictKnownMarketplaces, is
// refused by every strategy before any side effect.
func TestPolicyBlockedSource(t *testing.T) {
	Convey("Given blockedMarketplaces listing the source ref", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"blockedMarketplaces": ["`+policySourceRef+`"]}`)

		for _, strategy := range []host.Strategy{host.Native, host.Synth, host.Loose} {
			Convey("When "+string(strategy)+" delivers the blocked source", func() {
				runner := hostcli.NewScriptRunner(nil)
				h := host.NewClaude(host.WithHome(home), host.WithRunner(runner))

				pkg := policyPackage(t)
				pkg.Marketplace = policySourceRef
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy, AllowHooks: true})

				Convey("Then the cell is blocked by policy with the rule named", func() {
					typed, ok := errors.AsType[*host.PolicyError](err)
					So(ok, ShouldBeTrue)
					So(typed.Rule, ShouldEqual, "blockedMarketplaces")
					So(typed.Ref, ShouldEqual, policySourceRef)
					So(runner.Calls(), ShouldBeEmpty)
					So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
					So(fileExists(filepath.Join(home, ".claude", "agents")), ShouldBeFalse)
				})
			})
		}
	})

	Convey("Given strictKnownMarketplaces not listing the source ref", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"strictKnownMarketplaces": ["https://github.com/other/x.git"]}`)

		for _, strategy := range []host.Strategy{host.Native, host.Synth, host.Loose} {
			Convey("When "+string(strategy)+" delivers an unlisted source", func() {
				runner := hostcli.NewScriptRunner(nil)
				h := host.NewClaude(host.WithHome(home), host.WithRunner(runner))

				pkg := policyPackage(t)
				pkg.Marketplace = policySourceRef
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy, AllowHooks: true})

				Convey("Then the strict allowlist blocks it", func() {
					typed, ok := errors.AsType[*host.PolicyError](err)
					So(ok, ShouldBeTrue)
					So(typed.Rule, ShouldEqual, "strictKnownMarketplaces")
					So(runner.Calls(), ShouldBeEmpty)
				})
			})
		}
	})
}

// TestPolicySourceNotLocalPath pins that the marketplace lists match the source
// ref, never the local synth directory.
func TestPolicySourceNotLocalPath(t *testing.T) {
	Convey("Given policy lists naming the synth dir (a local path)", t, func() {
		fakeClaude(t)
		synthDir := t.TempDir()

		Convey("When a blocked list carries only the local paths", func() {
			st := openStore(t)
			pkg := synthPackage(t, st, "acme/caveman", "1.0.0")
			pkg.Marketplace = policySourceRef

			home := policyHome(t, `{"blockedMarketplaces": ["`+pkg.SynthDir+`", "`+ownerRoot(st, "acme")+`"]}`)
			cli := newClaudeCLI(filepath.Join(home, ".claude"))

			h := host.NewClaude(host.WithHome(home), host.WithRunner(cli), host.WithStore(st), host.WithTrash(st.Trash()))

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then local paths are not sources and the synth proceeds", func() {
				So(err, ShouldBeNil)

				_, installed := cli.Installed("caveman@acme")
				So(installed, ShouldBeTrue)
			})
		})

		Convey("When a strict allowlist carries only the local path", func() {
			home := policyHome(t, `{"strictKnownMarketplaces": ["`+synthDir+`"]}`)

			h, runner := newClaude(t, home, nil)

			pkg := policyPackage(t)
			pkg.Marketplace = policySourceRef
			pkg.SynthDir = synthDir

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Synth})

			Convey("Then the unlisted source ref is blocked, not the local path", func() {
				typed, ok := errors.AsType[*host.PolicyError](err)
				So(ok, ShouldBeTrue)
				So(typed.Rule, ShouldEqual, "strictKnownMarketplaces")
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})
}

// TestPolicyCommandSources pins `disableCommandPluginSources: true`: the CLI
// strata (native, synth) are blocked; loose is not a command source.
func TestPolicyCommandSources(t *testing.T) {
	Convey("Given disableCommandPluginSources", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"disableCommandPluginSources": true}`)

		Convey("When the CLI strata want an install", func() {
			pkg := policyPackage(t)
			pkg.Marketplace = policySourceRef

			assertCLIPolicyBlocked(t, home, pkg, func(runner *hostcli.ScriptRunner) host.Host {
				return host.NewClaude(host.WithHome(home), host.WithRunner(runner))
			})
		})

		Convey("When loose delivers the same package", func() {
			st := openStore(t)
			h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: policyPackage(t), Strategy: host.Loose})

			Convey("Then the non-CLI strata still deliver", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".claude", "skills", "alpha", "SKILL.md")), ShouldBeTrue)
			})
		})
	})
}

// TestPolicyManagedHooks pins `allowManagedHooksOnly: true`: plugin hooks are
// not delivered into the loose surface; the rest of the cell lands.
func TestPolicyManagedHooks(t *testing.T) {
	Convey("Given allowManagedHooksOnly", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"allowManagedHooksOnly": true}`)

		Convey("When loose would deliver plugin hooks", func() {
			st := openStore(t)
			h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

			res, err := h.Deliver(t.Context(), home, host.Delivery{Package: policyPackage(t), Strategy: host.Loose, AllowHooks: true})

			Convey("Then hooks are skipped with the rule named and the rest lands", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".claude", "skills", "alpha", "SKILL.md")), ShouldBeTrue)
				So(policyReadFile(t, filepath.Join(home, ".claude", "settings.json")), ShouldNotContainSubstring, `"hooks"`)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, "allowManagedHooksOnly")

				for _, artifact := range res.Artifacts {
					So(artifact.Kind, ShouldNotEqual, "hook")
				}
			})
		})

		Convey("When native installs (hooks are not part of the stratum)", func() {
			script := map[string]hostcli.Response{
				"claude plugin marketplace add " + policySourceRef: response(""),
				"claude plugin install caveman@plugins":            response(""),
				"claude plugin list --json":                        response(claudeList("caveman@plugins")),
			}

			h, runner := newClaude(t, home, script)

			pkg := policyPackage(t)
			pkg.Marketplace = policySourceRef

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

			Convey("Then the native install proceeds", func() {
				So(err, ShouldBeNil)
				So(callKeys(runner), ShouldHaveLength, 3)
			})
		})
	})
}

// TestPolicyUnreadableFailsClosed pins the fail-closed policy read.
func TestPolicyUnreadableFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny reads")
	}

	Convey("Given an unreadable policy document", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{}`)
		settings := filepath.Join(home, ".claude", "settings.json")

		if err := os.Chmod(settings, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		t.Cleanup(func() { _ = os.Chmod(settings, 0o600) })

		h, runner := newClaude(t, home, nil)

		pkg := policyPackage(t)
		pkg.Marketplace = policySourceRef

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Native})

		Convey("When the policy cannot be read", func() {
			Convey("Then delivery fails closed with a policy step error", func() {
				typed, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(typed.Step, ShouldEqual, "policy")
				So(runner.Calls(), ShouldBeEmpty)
			})
		})
	})
}

// TestPolicyEmptySourceRef pins decision Q4 (T1.6-review-1 [R4]):
// Package.Marketplace is the source ref the gate checks for every stratum; a
// delivery without one cannot satisfy an allowlist and is blocked (fail
// closed), while a blocklist alone has nothing to match and lets it through.
func TestPolicyEmptySourceRef(t *testing.T) {
	Convey("Given strictKnownMarketplaces and a package without a source ref", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"strictKnownMarketplaces": ["`+policySourceRef+`"]}`)

		for _, strategy := range []host.Strategy{host.Native, host.Synth, host.Loose} {
			Convey("When "+string(strategy)+" delivers it", func() {
				runner := hostcli.NewScriptRunner(nil)
				h := host.NewClaude(host.WithHome(home), host.WithRunner(runner))

				pkg := policyPackage(t)
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy})

				Convey("Then the allowlist blocks it before any call or write", func() {
					typed, ok := errors.AsType[*host.PolicyError](err)
					So(ok, ShouldBeTrue)
					So(typed.Rule, ShouldEqual, "strictKnownMarketplaces")
					So(typed.Ref, ShouldBeEmpty)
					So(err.Error(), ShouldContainSubstring, "without a source ref")
					So(runner.Calls(), ShouldBeEmpty)
					So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
				})
			})
		}
	})

	Convey("Given only blockedMarketplaces and a package without a source ref", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"blockedMarketplaces": ["`+policySourceRef+`"]}`)

		h, _ := newClaude(t, home, nil)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: policyPackage(t), Strategy: host.Loose})

		Convey("When loose delivers it", func() {
			Convey("Then the blocklist has nothing to match and the files land", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(home, ".claude", "skills", "alpha", "SKILL.md")), ShouldBeTrue)
			})
		})
	})
}

// TestPolicyStrictEmptyList pins NF-4: Claude documents
// `strictKnownMarketplaces: []` as complete lockdown, so the allowlist is gated
// by the key's presence — an empty list blocks every source on every stratum;
// a null value is no policy.
func TestPolicyStrictEmptyList(t *testing.T) {
	Convey("Given strictKnownMarketplaces: []", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"strictKnownMarketplaces": []}`)

		for _, strategy := range []host.Strategy{host.Native, host.Synth, host.Loose} {
			for _, ref := range []string{"", policySourceRef} {
				Convey("When "+string(strategy)+" delivers the ref "+ref, func() {
					runner := hostcli.NewScriptRunner(nil)
					h := host.NewClaude(host.WithHome(home), host.WithRunner(runner))

					pkg := policyPackage(t)
					pkg.Marketplace = ref
					pkg.SynthDir = t.TempDir()

					_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy})

					Convey("Then the empty allowlist blocks it before any call or write", func() {
						typed, ok := errors.AsType[*host.PolicyError](err)
						So(ok, ShouldBeTrue)
						So(typed.Rule, ShouldEqual, "strictKnownMarketplaces")
						So(runner.Calls(), ShouldBeEmpty)
						So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
					})
				})
			}
		}
	})

	Convey("Given strictKnownMarketplaces: null", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"strictKnownMarketplaces": null}`)

		h, _ := newClaude(t, home, nil)

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: policyPackage(t), Strategy: host.Loose})

		Convey("When loose delivers", func() {
			Convey("Then there is no allowlist", func() {
				So(err, ShouldBeNil)
			})
		})
	})
}

// TestPolicyStrategyValidatedFirst pins T1.6-review-1 [R5]: a strategy the
// adapter never delivers is refused as such, even when the policy would also
// block the source.
func TestPolicyStrategyValidatedFirst(t *testing.T) {
	Convey("Given blockedMarketplaces listing the source ref", t, func() {
		fakeClaude(t)
		home := policyHome(t, `{"blockedMarketplaces": ["`+policySourceRef+`"]}`)

		// Silenced is not one of these: it is a rung the ladder can now choose,
		// so it is a delivered outcome rather than a strategy the adapter never
		// delivers. Using it here would have tested the old contract and kept
		// passing for the wrong reason after the change.
		for _, strategy := range []host.Strategy{"bogus", "not-a-strategy"} {
			Convey("When the strategy "+string(strategy)+" is requested", func() {
				h, runner := newClaude(t, home, nil)

				pkg := policyPackage(t)
				pkg.Marketplace = policySourceRef

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy})

				Convey("Then the adapter refuses the strategy, not the source", func() {
					_, ok := errors.AsType[*host.UnsupportedStrategyError](err)
					So(ok, ShouldBeTrue)
					So(runner.Calls(), ShouldBeEmpty)
				})
			})
		}
	})
}

// TestPolicyMalformedListsFailClosed pins T1.6-review-1 [R9]: a present but
// non-array blockedMarketplaces or strictKnownMarketplaces cannot be applied,
// so the gate fails closed instead of ignoring it.
func TestPolicyMalformedListsFailClosed(t *testing.T) {
	for _, doc := range []string{
		`{"blockedMarketplaces": "` + policySourceRef + `"}`,
		`{"strictKnownMarketplaces": {"source": "` + policySourceRef + `"}}`,
	} {
		Convey("Given the policy document "+doc, t, func() {
			fakeClaude(t)
			home := policyHome(t, doc)

			h, runner := newClaude(t, home, nil)

			pkg := policyPackage(t)
			pkg.Marketplace = policySourceRef

			_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("When loose delivers the source", func() {
				Convey("Then it fails closed with a policy step error and writes nothing", func() {
					typed, ok := errors.AsType[*host.DeliveryError](err)
					So(ok, ShouldBeTrue)
					So(typed.Step, ShouldEqual, "policy")
					So(runner.Calls(), ShouldBeEmpty)
					So(fileExists(filepath.Join(home, ".claude", "skills")), ShouldBeFalse)
				})
			})
		})
	}
}

// TestLooseJSONCSettingsHooks pins B2: a commented settings.json still accepts
// hooks delivery and keeps the comment (DESIGN §4.3).
func TestLooseJSONCSettingsHooks(t *testing.T) {
	Convey("Given a settings.json with a user comment", t, func() {
		fakeClaude(t)
		home := t.TempDir()
		writeSettings(t, home, "{\n  // user comment\n  \"model\": \"opus\"\n}\n")

		st := openStore(t)
		h, _ := newClaude(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: policyPackage(t), Strategy: host.Loose, AllowHooks: true})

		Convey("When hooks are delivered", func() {
			data := policyReadFile(t, filepath.Join(home, ".claude", "settings.json"))

			Convey("Then the comment survives and the hooks land", func() {
				So(err, ShouldBeNil)
				So(data, ShouldContainSubstring, "// user comment")
				So(data, ShouldContainSubstring, `"model": "opus"`)
				So(data, ShouldContainSubstring, `"hooks"`)
				So(digestOf(t, res, "", filepath.Join(home, ".claude", "settings.json")), ShouldNotBeEmpty)
			})
		})
	})
}
