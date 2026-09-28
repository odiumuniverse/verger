package host_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/digest"
	"github.com/odiumuniverse/verger/pkg/host"
	"github.com/odiumuniverse/verger/pkg/hostcli"
	"github.com/odiumuniverse/verger/pkg/manifest"
	"github.com/odiumuniverse/verger/pkg/receipt"
	"github.com/odiumuniverse/verger/pkg/render"
	"github.com/odiumuniverse/verger/pkg/secret"
)

// dshFixtureRoot is the DeepSeek Harness fixture package.
const dshFixtureRoot = "testdata/dsh/acme"

// fakeDSH puts an executable `dsh` shim at the front of PATH.
func fakeDSH(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "dsh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a fake host CLI must be executable
		t.Fatalf("write shim: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// newDSH builds the DeepSeek Harness adapter with a scripted runner.
func newDSH(t *testing.T, home string, script map[string]hostcli.Response, opts ...host.Option) (host.Host, *hostcli.ScriptRunner) {
	t.Helper()

	runner := hostcli.NewScriptRunner(script)
	all := append([]host.Option{host.WithHome(home), host.WithRunner(runner)}, opts...)

	return host.NewDSH(all...), runner
}

// dshPackage parses the DSH fixture and adds the rule component the manifest
// format does not carry.
func dshPackage(t *testing.T) host.Package {
	t.Helper()

	return adapterFixturePackage(t, dshFixtureRoot, manifest.FormatClaude)
}

// clearDSHEnv removes the DSH relocations, so a test that does not exercise
// them sees the default home.
func clearDSHEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{"DSH_HOME", "DSH_AGENTS_HOME"} {
		t.Setenv(name, "")
	}
}

// dshSecrets is the secret store of the DSH fixture: its MCP servers reference
// MCP_TOKEN and WEB_TOKEN.
func dshSecrets(t *testing.T) *secret.Store {
	t.Helper()

	return secretStore(t, map[string]string{"MCP_TOKEN": "s3cr3t-token", "WEB_TOKEN": "web-token"})
}

// dshA41Reason is beadle's documented no-go for DSH subagents, permissions and
// slash commands (decision A-41): in DSH they are runtime state and code, not
// files.
const dshA41Reason = "runtime state and code, not files"

// TestDSHDetect pins the beadle detect rule (pkg/agent/dsh.go): the DSH home
// exists or the `dsh` binary is on PATH.
func TestDSHDetect(t *testing.T) {
	Convey("Given a home without DSH", t, func() {
		home := t.TempDir()
		t.Setenv("PATH", t.TempDir())
		clearDSHEnv(t)

		h, _ := newDSH(t, home, nil)

		Convey("When neither the home nor the binary exist", func() {
			Convey("Then Detect is false", func() {
				So(h.Detect(home), ShouldBeFalse)
			})
		})

		Convey("When ~/.dsh exists", func() {
			if err := os.MkdirAll(filepath.Join(home, ".dsh"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})

		Convey("When DSH_HOME relocates the home", func() {
			elsewhere := t.TempDir()
			t.Setenv("DSH_HOME", elsewhere)

			Convey("Then Detect follows the relocation", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})

	Convey("Given the dsh binary on PATH", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		h, _ := newDSH(t, home, nil)

		Convey("When only the binary exists", func() {
			Convey("Then Detect is true", func() {
				So(h.Detect(home), ShouldBeTrue)
			})
		})
	})
}

// TestDSHLooseGolden pins the three DSH surfaces beadle verified: skills in
// $DSH_HOME/skills, MCP in the $DSH_HOME/cordis.patch.yml home layer, and
// rules without a file surface — everything else is a documented no-go.
func TestDSHLooseGolden(t *testing.T) {
	Convey("Given a DSH package and a temp home", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		st := openStore(t)
		pkg := dshPackage(t)

		h, runner := newDSH(t, home, nil,
			host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(dshSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose, AllowHooks: true})

		Convey("When it is delivered loose", func() {
			Convey("Then skills, the rule wrapper and the home patch layer land; nothing else", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldResemble, []string{
					".dsh/cordis.patch.yml",
					".dsh/skills/alpha/SKILL.md",
					".dsh/skills/alpha/scripts/run.sh",
					".dsh/skills/beta/SKILL.md",
					".dsh/skills/rule-caveman/SKILL.md",
				})
				So(runner.Calls(), ShouldBeEmpty)
			})

			Convey("Then every component DSH cannot take is named with its reason", func() {
				notes := strings.Join(res.Notes, "\n")
				So(notes, ShouldContainSubstring, `agent component "reviewer" is not expressible in loose dsh`)
				So(notes, ShouldContainSubstring, dshA41Reason)
				So(notes, ShouldContainSubstring, `command "dev" skipped`)
				So(notes, ShouldContainSubstring, "1 hook(s) skipped")
				So(notes, ShouldContainSubstring, "rule is delivered as a skill wrapper")
			})

			Convey("Then the MCP servers land in the home patch layer as DSH mount records", func() {
				patch := readTestFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml"))

				So(patch, ShouldContainSubstring, "- insert:")
				So(patch, ShouldContainSubstring, "id: verger:fs")
				So(patch, ShouldContainSubstring, "id: verger:web")
				So(patch, ShouldContainSubstring, "name: '@deepseek-ai/dsh-mcp-client'")
				So(patch, ShouldContainSubstring, "transport: stdio")
				So(patch, ShouldContainSubstring, "transport: streamable-http")
				So(patch, ShouldContainSubstring, "serverName: fs")
				So(patch, ShouldContainSubstring, "serverName: web")
				So(patch, ShouldContainSubstring, "command: node")
				So(patch, ShouldContainSubstring, "args:")
				So(patch, ShouldContainSubstring, pkg.Root+"/server.js")
				So(patch, ShouldContainSubstring, "TOKEN: s3cr3t-token")
				So(patch, ShouldContainSubstring, "url: https://example.com/mcp")
				So(patch, ShouldContainSubstring, "Authorization: Bearer web-token")
			})

			Convey("Then the RMA carries a file op per artifact and no host-install op", func() {
				assertArtifactsBackedByOps(t, res)

				for _, op := range res.RMA {
					So(op.Kind, ShouldNotEqual, receipt.OpHostInstall)
				}
			})
		})
	})
}

// TestDSHHomeEnv pins beadle's literal DSH_HOME rule: a non-empty value is
// taken as written (no trim, no ~ expansion), an empty value falls back to
// ~/.dsh (pkg/agent/dsh.go).
func TestDSHHomeEnv(t *testing.T) {
	Convey("Given DSH_HOME pointing elsewhere", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		elsewhere := t.TempDir()
		t.Setenv("DSH_HOME", elsewhere)

		h, _ := newDSH(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(dshSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then every surface lands under the relocated home and ~/.dsh stays untouched", func() {
				So(err, ShouldBeNil)
				So(fileExists(filepath.Join(elsewhere, "skills", "alpha", "SKILL.md")), ShouldBeTrue)
				So(fileExists(filepath.Join(elsewhere, "cordis.patch.yml")), ShouldBeTrue)
				So(fileExists(filepath.Join(home, ".dsh")), ShouldBeFalse)
			})
		})
	})
}

// TestDSHAgentsHomeIsReadOnly pins that the shared agents home (rank 500, which
// DSH reads after its own) is never written: beadle treats it as shared and
// shadowed, and so does verger.
func TestDSHAgentsHomeIsReadOnly(t *testing.T) {
	Convey("Given DSH_AGENTS_HOME pointing elsewhere", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		shared := t.TempDir()
		t.Setenv("DSH_AGENTS_HOME", shared)

		h, _ := newDSH(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(dshSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then nothing is written into the shared agents home", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, shared), ShouldBeEmpty)
			})
		})
	})
}

// TestDSHPatchPreservesForeignEntries pins that a hand-written patch entry and
// its comments survive a delivery: verger owns only the records whose id
// carries its prefix.
func TestDSHPatchPreservesForeignEntries(t *testing.T) {
	Convey("Given a hand-written home patch layer", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		writeFixtureFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml"), `# my own layer
- insert:
    - id: mine
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        transport: stdio
        serverName: mine
        command: /bin/echo
- disable:
    id: other
`, 0o600)

		st := openStore(t)
		h, _ := newDSH(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()), host.WithSecrets(dshSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the foreign entry and the comment stay byte-for-byte", func() {
				So(err, ShouldBeNil)

				patch := readTestFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml"))
				So(patch, ShouldContainSubstring, "# my own layer")
				So(patch, ShouldContainSubstring, "id: mine")
				So(patch, ShouldContainSubstring, "serverName: mine")
				So(patch, ShouldContainSubstring, "- disable:")
				So(patch, ShouldContainSubstring, "id: verger:fs")
			})
		})
	})
}

// TestDSHPatchEmptyIsRefused pins the loader's own rule: DSH needs a top-level
// YAML array, so an empty patch file is reported instead of being overwritten.
func TestDSHPatchEmptyIsRefused(t *testing.T) {
	Convey("Given an empty home patch layer", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		writeFixtureFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml"), "\n", 0o600)

		h, _ := newDSH(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(dshSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the cell fails and the file is left alone", func() {
				delivery, ok := errors.AsType[*host.DeliveryError](err)
				So(ok, ShouldBeTrue)
				So(delivery.Host, ShouldEqual, "dsh")
				So(delivery.Cause, ShouldBeError)
				So(readTestFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml")), ShouldEqual, "\n")
			})
		})
	})
}

// TestDSHPatchMappingIsRefused pins the same rule for a document whose top
// level is not an array: DSH would not read the records.
func TestDSHPatchMappingIsRefused(t *testing.T) {
	Convey("Given a home patch layer that is a mapping", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		writeFixtureFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml"), "mcpServers:\n  fs: {}\n", 0o600)

		h, _ := newDSH(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(dshSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the cell fails instead of writing records DSH cannot mount", func() {
				So(err, ShouldBeError)
				So(readTestFile(t, filepath.Join(home, ".dsh", "cordis.patch.yml")), ShouldEqual, "mcpServers:\n  fs: {}\n")
			})
		})
	})
}

// TestDSHPatchServerNameRefused pins the loader's own constraint: the record's
// serverName becomes part of every tool name and must match
// [A-Za-z0-9_-]{1,32}.
func TestDSHPatchServerNameRefused(t *testing.T) {
	Convey("Given an MCP server whose name DSH cannot mount", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		pkg := dshPackage(t)
		pkg.MCP = []manifest.MCPServer{{Name: "not a name", Transport: "stdio", Command: []string{"node", "s.js"}}}

		h, _ := newDSH(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(dshSecrets(t)))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the cell fails and no patch file is created", func() {
				So(err, ShouldBeError)
				So(fileExists(filepath.Join(home, ".dsh", "cordis.patch.yml")), ShouldBeFalse)
			})
		})
	})
}

// TestDSHPatchReDelivery pins the idempotent re-install: the records are already
// exactly as wanted, so no second write happens and the keys stay owned.
func TestDSHPatchReDelivery(t *testing.T) {
	Convey("Given a delivered DSH package", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		st := openStore(t)
		pkg := dshPackage(t)

		h, _ := newDSH(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
			host.WithSecrets(dshSecrets(t)), host.WithOwnership(ownerExisting("acme/caveman")))

		_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})
		So(err, ShouldBeNil)

		patch := filepath.Join(home, ".dsh", "cordis.patch.yml")
		before := readTestFile(t, patch)

		Convey("When it is delivered again", func() {
			again, againErr := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: host.Loose})

			Convey("Then the patch layer is unchanged and the document and each record stay owned", func() {
				So(againErr, ShouldBeNil)
				So(readTestFile(t, patch), ShouldEqual, before)

				// A patch layer is a YAML array of loader records, not a
				// key-path document, so the document itself is owned as one
				// file (a removal restores the trashed bytes instead of
				// unsetting keys that mean nothing to the loader), and every
				// record carries its own value digest for the next delivery.
				var (
					files  []receipt.Op
					owned  = map[string]receipt.Op{}
					claims = map[string]digest.Hash{}
				)

				for _, op := range again.RMA {
					switch op.Kind {
					case receipt.OpWriteFile:
						if op.Path == patch {
							files = append(files, op)
						}
					case receipt.OpRecord:
						So(op.Note, ShouldEqual, patch)

						owned[strings.TrimPrefix(op.Path, "dsh://patch/")] = op
					case receipt.OpCopyTree, receipt.OpSymlink, receipt.OpHardlink,
						receipt.OpConfigKey, receipt.OpHostInstall:
						// Skills and trees are claimed by their own paths; the
						// patch document carries none of these.
					}
				}

				So(files, ShouldHaveLength, 1)
				So(files[0].Existed, ShouldBeTrue)
				So(files[0].Backup, ShouldBeEmpty)

				for _, artifact := range again.Artifacts {
					claims[artifact.Path] = artifact.Digest
				}

				So(owned, ShouldHaveLength, 2)

				for name, op := range owned {
					So(op.Digest.Valid(), ShouldBeTrue)
					So(claims["dsh://patch/"+name], ShouldEqual, op.Digest)
				}
			})
		})
	})
}

// TestDSHNoInstaller pins the strategy ladder of a host whose `dsh plugin`
// verb is a pnpm passthrough: native and synth are refused with the reason and
// no CLI call runs.
func TestDSHNoInstaller(t *testing.T) {
	Convey("Given a package with a marketplace ref", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		pkg := dshPackage(t)
		pkg.Marketplace = "https://github.com/acme/plugins.git"

		for _, strategy := range []host.Strategy{host.Native, host.Synth} {
			Convey("When "+string(strategy)+" is requested", func() {
				h, runner := newDSH(t, home, nil)

				pkg := pkg
				pkg.SynthDir = t.TempDir()

				_, err := h.Deliver(t.Context(), home, host.Delivery{Package: pkg, Strategy: strategy})

				Convey("Then it is refused with the missing installer named", func() {
					unsupported, ok := errors.AsType[*host.NotSupportedError](err)
					So(ok, ShouldBeTrue)
					So(unsupported.Operation, ShouldContainSubstring, "dsh plugin")
					So(unsupported.Operation, ShouldContainSubstring, "pnpm")
					So(callKeys(runner), ShouldBeEmpty)
				})
			})
		}
	})
}

// TestDSHUninstall pins the removal of a file-only receipt: DSH records no
// host-install op, so nothing is run against the host.
func TestDSHUninstall(t *testing.T) {
	Convey("Given a DSH receipt without a host-install op", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		h, runner := newDSH(t, home, nil)

		r := receipt.Receipt{Strategy: string(host.Loose), RMA: []receipt.Op{
			{Kind: receipt.OpWriteFile, Path: filepath.Join(t.TempDir(), "skill.md")},
		}}

		res, err := h.Uninstall(t.Context(), "", r)

		Convey("When it is uninstalled", func() {
			Convey("Then the host is never called and there is nothing to note", func() {
				So(err, ShouldBeNil)
				So(res.Notes, ShouldBeEmpty)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestDSHOracleUnsupported pins the honest verdict: `dsh plugin` is a pnpm
// passthrough and the host has no listing verb, so both oracle calls report
// that the host cannot answer rather than shelling out to something else.
func TestDSHOracleUnsupported(t *testing.T) {
	Convey("Given the DSH oracle", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		h, runner := newDSH(t, home, nil)

		warnings, err := h.Oracle().Validate(t.Context(), t.TempDir())

		Convey("When the host is asked to list or validate", func() {
			Convey("Then both are unsupported and no CLI call is made", func() {
				So(errors.Is(err, host.ErrNotSupported), ShouldBeTrue)
				So(warnings, ShouldBeEmpty)

				_, listErr := h.Oracle().List(t.Context())
				So(errors.Is(listErr, host.ErrNotSupported), ShouldBeTrue)
				So(callKeys(runner), ShouldBeEmpty)
			})
		})
	})
}

// TestDSHLooseDryRunWritesNothing pins the dry-run contract: the plan is
// returned and nothing is written, so the real pass does not meet the dry pass's
// own output as a foreign path.
func TestDSHLooseDryRunWritesNothing(t *testing.T) {
	Convey("Given a DSH package and a temp home", t, func() {
		fakeDSH(t)

		home := t.TempDir()
		clearDSHEnv(t)

		h, _ := newDSH(t, home, nil, host.WithStore(openStore(t)), host.WithSecrets(dshSecrets(t)))

		res, err := h.Deliver(t.Context(), home, host.Delivery{Package: dshPackage(t), Strategy: host.Loose, DryRun: true})

		Convey("When the delivery is a dry run", func() {
			Convey("Then the plan is returned and the home is untouched", func() {
				So(err, ShouldBeNil)
				So(homeFiles(t, home), ShouldBeEmpty)
				So(res.RMA, ShouldNotBeEmpty)
				So(strings.Join(res.Notes, "\n"), ShouldContainSubstring, noteDryRunText)
			})
		})
	})
}

// dshRecordValue digests one record config exactly as the adapter does: the
// canonical JSON of the `config` mapping.
func dshRecordValue(t *testing.T, config map[string]any) digest.Hash {
	t.Helper()

	data, err := json.Marshal(config)
	So(err, ShouldBeNil)

	return digest.Bytes(data)
}

// recordDigests is a PathOwner that owns nothing but reports the recorded
// digest of the paths in sums — what the previous receipt proved about them.
type recordDigests struct {
	sums map[string]digest.Hash
}

// Owner implements host.PathOwner: a record identity is not a filesystem path,
// so ownership is reported for exactly the paths the test seeds.
func (o recordDigests) Owner(path string) (string, bool) {
	if _, ok := o.sums[path]; !ok {
		return "", false
	}

	return "acme/caveman", true
}

// ArtifactDigest implements host.ArtifactDigests.
func (o recordDigests) ArtifactDigest(path string) (digest.Hash, bool) {
	sum, ok := o.sums[path]

	return sum, ok
}

// dshRecordHome seeds a home with a hand-written home patch layer and returns the
// adapter's delivery dependencies over a store and a secret store.
func dshRecordHome(t *testing.T, body string, sums map[string]digest.Hash) (string, host.Host) {
	t.Helper()

	home := t.TempDir()

	clearDSHEnv(t)

	patch := filepath.Join(home, ".dsh", "cordis.patch.yml")

	if body != "" {
		writeFixtureFile(t, patch, body, 0o600)
	}

	st := openStore(t)
	h, _ := newDSH(t, home, nil, host.WithStore(st), host.WithTrash(st.Trash()),
		host.WithOwnership(recordDigests{sums: sums}), host.WithSecrets(dshSecrets(t)))

	return patch, h
}

// TestDSHOwnedRecordIsUpdated pins the first of the three ownership cases: a
// record whose value still matches the digest the previous receipt recorded for
// it is verger's, so the delivery rewrites it in place instead of appending a
// second record.
func TestDSHOwnedRecordIsUpdated(t *testing.T) {
	Convey("Given a patch layer whose record verger owns unchanged", t, func() {
		fakeDSH(t)

		seeded := `# layer
- insert:
    - id: verger:fs
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        transport: stdio
        serverName: fs
        command: node
        args:
          - old-server.js
`

		patch, h := dshRecordHome(t, seeded, map[string]digest.Hash{
			"dsh://patch/fs": dshRecordValue(t, map[string]any{
				"transport": "stdio", "serverName": "fs", "command": "node",
				"args": []any{"old-server.js"},
			}),
		})

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the record is updated in place, not appended, and the comment survives", func() {
				So(err, ShouldBeNil)

				updated := readTestFile(t, patch)
				So(updated, ShouldContainSubstring, "# layer")
				So(updated, ShouldContainSubstring, "testdata/dsh/acme/server.js")
				So(updated, ShouldNotContainSubstring, "old-server.js")
				So(strings.Count(updated, "id: verger:fs"), ShouldEqual, 1)
				So(updated, ShouldContainSubstring, "id: verger:web")
			})
		})
	})
}

// TestDSHHandEditedRecordIsHandsOff pins the second case: the user edited the
// record by hand, so it is no longer the value verger wrote — the cell is
// hands-off and not one byte of the layer moves.
func TestDSHHandEditedRecordIsHandsOff(t *testing.T) {
	Convey("Given a patch layer whose record was edited by hand", t, func() {
		fakeDSH(t)

		handEdited := `# layer
- insert:
    - id: verger:fs
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        transport: stdio
        serverName: fs
        command: /usr/local/bin/my-own-fs
`

		patch, h := dshRecordHome(t, handEdited, map[string]digest.Hash{
			"dsh://patch/fs": dshRecordValue(t, map[string]any{
				"transport": "stdio", "serverName": "fs", "command": "node",
				"args": []any{"testdata/dsh/acme/server.js"}, "env": map[string]any{"TOKEN": "s3cr3t-token"},
			}),
		})

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the record is hands-off and the layer is untouched", func() {
				hands, ok := errors.AsType[*render.HandsOffError](err)
				So(ok, ShouldBeTrue)
				So(hands.KeyPath, ShouldEqual, "mcpServers.fs")
				So(hands.Reason, ShouldContainSubstring, "changed outside verger")
				So(readTestFile(t, patch), ShouldEqual, handEdited)
			})
		})
	})
}

// TestDSHForeignRecordIsNeverTouched pins the third case: a record verger never
// wrote carries no id prefix, so the delivery neither reads it for writing nor
// changes it — even when it describes the same serverName the package wants.
func TestDSHForeignRecordIsNeverTouched(t *testing.T) {
	Convey("Given a patch layer verger never wrote a record into", t, func() {
		fakeDSH(t)

		patch, h := dshRecordHome(t, `# layer
- insert:
    - id: mine
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        transport: stdio
        serverName: fs
        command: /bin/echo
`, nil)

		_, err := h.Deliver(t.Context(), "", host.Delivery{Package: dshPackage(t), Strategy: host.Loose})

		Convey("When the package is delivered", func() {
			Convey("Then the foreign record keeps its id and config and only new records are added", func() {
				So(err, ShouldBeNil)

				updated := readTestFile(t, patch)
				So(updated, ShouldContainSubstring, "# layer")
				So(updated, ShouldContainSubstring, "- id: mine")
				So(updated, ShouldContainSubstring, "command: /bin/echo")
				So(updated, ShouldContainSubstring, "id: verger:fs")
				So(updated, ShouldContainSubstring, "id: verger:web")
			})
		})
	})
}
