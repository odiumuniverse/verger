package hostpath_test

import (
	"path/filepath"
	"slices"
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/odiumuniverse/verger/pkg/hostpath"
)

// TestAnEmptyVariableIsUnset pins the rule the XDG Base Directory specification
// states and the hosts implement: an empty value is indistinguishable from an
// unset one, so `FOO=` is a caller that has not asked for anything.
//
// The gate lives in hostpath rather than in pkg/home, and that placement is the
// point. pkg/home takes a lookup that hands back a plain string, so unset and
// empty are the same INPUT there and there is nothing to tell apart. hostpath's
// Lookup answers (value, ok), so "SET to empty" is expressible - and a resolver
// that read the `ok` instead of the value would look for a root of "", which is a
// RELATIVE path: it resolves against whatever directory the user happened to be
// standing in, and a delivery into it succeeds and is invisible.
//
// A variable SET to "" must therefore never produce a root built from "".
func TestAnEmptyVariableIsUnset(t *testing.T) {
	variables := []string{
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GEMINI_CLI_HOME", "CURSOR_CONFIG_DIR",
		"OPENCODE_CONFIG_DIR", "KILO_CONFIG_DIR", "PI_CODING_AGENT_DIR", "DSH_HOME",
	}

	// Both platforms verger ships on, and GOOS is an INPUT to the resolver rather
	// than a build tag: the rule has to hold for every value the resolver is
	// handed. A resolver that behaved only on the machine it was written on would
	// pass this test on exactly that machine.
	for _, goos := range []string{"linux", "darwin"} {
		Convey("Given every variable that can name a root, SET to the empty string, on "+goos, t, func() {
			for _, id := range hostpath.All() {
				Convey("When "+id+" resolves with that variable empty on "+goos, func() {
					empty := func(name string) (string, bool) {
						if slices.Contains(variables, name) {
							return "", true // SET, and empty
						}

						return "", false
					}

					roots, err := hostpath.Roots(id, hostpath.Env{
						Home: "/home/u", GOOS: goos, Lookup: empty,
					})
					So(err, ShouldBeNil)

					Convey("Then no root is relative or empty", func() {
						// Every root must be absolute. A "" leaking into a join gives
						// the bare app name - relative, and writable from wherever the
						// process happened to start.
						for name, path := range map[string]string{
							"ConfigRoot":   roots.ConfigRoot,
							"StateRoot":    roots.StateRoot,
							"SettingsRoot": roots.SettingsRoot,
						} {
							if path == "" {
								continue
							}

							// The root's own NAME is in the failure on purpose: a bare
							// path tells you it is relative, not which root is.
							SoMsg(name+" must be absolute", path, ShouldStartWith, "/")
							SoMsg(name+" must not be a bare app name", path, ShouldNotEqual, filepath.Base(path))
						}
					})

					Convey("Then the root is the same one an UNSET variable gives", func() {
						// The whole claim in one assertion: empty and unset are the
						// same input, so the resolver must answer the same both ways.
						unset, unsetErr := hostpath.Roots(id, hostpath.Env{
							Home: "/home/u", GOOS: goos,
							Lookup: func(string) (string, bool) { return "", false },
						})
						So(unsetErr, ShouldBeNil)
						So(roots.ConfigRoot, ShouldEqual, unset.ConfigRoot)
						So(roots.StateRoot, ShouldEqual, unset.StateRoot)
					})
				})
			}
		})
	}
}
