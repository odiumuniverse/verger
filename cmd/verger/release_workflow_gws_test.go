package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
	"go.yaml.in/yaml/v3"
)

// The publish job computes the three digests the formula needs, and a pipeline's
// exit status is the exit status of its *last* command. The digests are read as
// `sha256sum "dist/…" | cut -d' ' -f1`, so a missing artefact makes sha256sum
// fail, leaves cut exiting 0, and assigns an empty string — and the release goes
// on to commit a formula whose sha256 is "". Homebrew then fails much later, on a
// user's machine, with an error that points at the tap rather than at a tarball
// that was never built.
//
// The step is checked by running it, not by reading it: the digest half of the
// real step, extracted with a YAML parser, against a dist/ that has nothing in
// it. It has to fail, and it has to say which file it wanted.
func TestTheFormulaStepRefusesToRenderWithoutItsArtifacts(t *testing.T) {
	Convey("Given the publish step's digest half and a dist with no artifacts in it", t, func() {
		script := digestHalf(t, "../../.github/workflows/release.yml")

		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "dist"), 0o700); err != nil {
			t.Fatal(err)
		}

		path := filepath.Join(home, "digest.sh")
		if err := os.WriteFile(path, []byte("set -e\n"+script), 0o600); err != nil {
			t.Fatal(err)
		}

		// `sh path`, not `path`: the runner's step is a shell script, and
		// handing it to sh needs no executable bit, so the file can be written
		// 0600 like everything else a test writes.
		cmd := exec.CommandContext(t.Context(), "sh", path) //nolint:gosec // G204: `sh` is a literal and path is this test's own extraction
		cmd.Dir = home
		// The step refuses early without a token, which is its own contract and
		// not what this test is about; a dummy gets past it so the digests run.
		cmd.Env = append(os.Environ(), "VERSION=0.1.2", "TAP_TOKEN=dummy")

		out, err := cmd.CombinedOutput()

		Convey("Then the step fails instead of writing an empty digest into the formula", func() {
			So(err, ShouldNotBeNil)
			// Whichever artefact it notices first, it has to name it: "something
			// is missing" is the difference between a release that stops here and
			// one that ships sha256 "".
			So(string(out), ShouldContainSubstring, ".tar.gz is missing")
			So(string(out), ShouldContainSubstring, "::error::")
		})

		Convey("Then it never reaches the tap", func() {
			So(string(out), ShouldNotContainSubstring, "Cloning into")
		})
	})
}

// digestHalf is the part of the "Bump the formula in homebrew-tap" step that runs
// before it clones anything: the version, the token check and the three digests.
// Slicing there is what keeps the test away from the network and from the push —
// the failure this is about happens entirely before either.
func digestHalf(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the workflow this test checks
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("release.yml is not valid YAML: %v", err)
	}

	publish, ok := doc.Jobs["publish"]
	if !ok {
		t.Fatal("release.yml has no publish job")
	}

	for _, step := range publish.Steps {
		if step.Name != "Bump the formula in homebrew-tap" {
			continue
		}

		lines := strings.Split(step.Run, "\n")

		for i, line := range lines {
			if strings.Contains(line, "git clone") {
				return strings.Join(lines[:i], "\n")
			}
		}

		t.Fatal("the step no longer clones; the harness cannot tell where the digests end")
	}

	t.Fatal("release.yml has no step named \"Bump the formula in homebrew-tap\"")

	return ""
}
