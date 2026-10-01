package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
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

// The version the harness pretends the workflow resolved. It stands in for the
// `$VERSION` the steps interpolate, so a filename built from it is the
// filename the runner would really have.
const harnessVersion = "0.0.0-harness"

// checksums.txt is the only thing a user can check a download against, and it
// is written before the publish step runs. If it names a file the release does
// not publish, `shasum -c checksums.txt` fails for every user who tries it; if
// it omits a file the release does publish, that download is unverifiable. Both
// directions are checked here, and neither is checked by reading: the
// checksummed set is produced by RUNNING the checksum step over a dist holding
// exactly the tarballs the build steps create, and the published set is
// resolved from the upload globs the publish job hands to `gh release upload`.
func TestChecksumsCoverExactlyThePublishedArtifacts(t *testing.T) {
	wf := loadWorkflow(t, "../../.github/workflows/release.yml")

	Convey("Given the release workflow", t, func() {
		build := wf.job(t, "build")
		publish := wf.job(t, "publish")

		Convey("When the checksums are generated over the artifacts the build steps build", func() {
			built := build.artifactsBuilt(t)
			So(built, ShouldNotBeEmpty)

			checksummed := build.generatedChecksums(t, built)
			So(checksummed, ShouldNotBeEmpty)

			Convey("Then no artifact is built that never reaches a release asset", func() {
				// A dead artifact in checksums.txt is the defect in its purest
				// form: it is named, and a user who downloads the release to
				// verify it cannot get it.
				So(built, ShouldResemble, checksummed)
			})

			Convey("Then the release publishes every checksummed file, and nothing else", func() {
				published := publish.publishedArtifacts(t, build.uploaded(t, built))

				// checksums.txt itself is the checksum file: it cannot carry its
				// own digest, so it is the one published asset exempt from being
				// listed in it.
				So(published, ShouldContain, "checksums.txt")
				So(
					sortedUnique(withOut(published, "checksums.txt")),
					ShouldResemble,
					sortedUnique(checksummed),
				)
			})
		})
	})
}

// workflowStep is the shape of a step this test reads. `with` holds the inputs
// of an action step, `run` the shell of a script step.
type workflowStep struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
	With struct {
		Name string `yaml:"name"`
		Path string `yaml:"path"`
	} `yaml:"with"`
}

type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

// releaseWorkflow is release.yml, parsed.
type releaseWorkflow struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, path string) *releaseWorkflow {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // G304: the workflow this test checks
	if err != nil {
		t.Fatal(err)
	}

	wf := &releaseWorkflow{}
	if err := yaml.Unmarshal(data, wf); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}

	return wf
}

func (w *releaseWorkflow) job(t *testing.T, name string) workflowJob {
	t.Helper()

	job, ok := w.Jobs[name]
	if !ok {
		t.Fatalf("release.yml has no %s job", name)
	}

	return job
}

// step returns the named step, failing the test when it is gone: a rename that
// leaves this test unable to find the step is a broken test, not a pass.
func (j workflowJob) step(t *testing.T, name string) workflowStep {
	t.Helper()

	for _, step := range j.Steps {
		if step.Name == name {
			return step
		}
	}

	t.Fatalf("release.yml has no step named %q", name)

	return workflowStep{}
}

// tarballRE pulls the artifact name out of every `tar -czf "dist/NAME"` the
// build steps run. Reading the build steps rather than hard-coding the three
// platforms is what makes "built" the real set: adding a tarball to the build
// adds it here too.
var tarballRE = regexp.MustCompile(`tar\s+-czf\s+"?dist/([^"\s]+)"?`)

// artifactsBuilt is every file the build job leaves in dist/, as the runner
// would name it with the version resolved.
func (j workflowJob) artifactsBuilt(t *testing.T) []string {
	t.Helper()

	var built []string

	for _, step := range j.Steps {
		for _, match := range tarballRE.FindAllStringSubmatch(step.Run, -1) {
			built = append(built, withVersion(match[1]))
		}
	}

	return sortedUnique(built)
}

// generatedChecksums runs the checksum step over a dist holding exactly the
// given artifacts and returns the file names it listed, one per line of the
// checksums.txt it produced. Running it is the point: a hand-maintained list of
// names inside the step would produce a different checksums.txt than the glob
// does, and this is what catches the two drifting apart.
func (j workflowJob) generatedChecksums(t *testing.T, artifacts []string) []string {
	t.Helper()

	step := j.step(t, "Checksum the artifacts")
	if step.Run == "" {
		t.Fatal("the \"Checksum the artifacts\" step does not run a script")
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, name := range artifacts {
		// Content is irrelevant to a checksum: the step only needs the files to
		// exist, and an empty file is a legitimate artifact as far as that step
		// is concerned.
		if err := os.WriteFile(filepath.Join(home, "dist", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(home, "checksums.sh")
	if err := os.WriteFile(path, []byte("set -e\n"+step.Run), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", path) //nolint:gosec // G204: `sh` is a literal and path is this test's own extraction
	cmd.Dir = home

	cmd.Env = append(os.Environ(), "VERSION="+harnessVersion, "PATH="+shimPath(t, os.Getenv("PATH")))

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the checksum step failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(home, "dist", "checksums.txt")) //nolint:gosec // G304: this test's own temp dir
	if err != nil {
		t.Fatalf("the checksum step wrote no checksums.txt: %v", err)
	}

	var listed []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		_, name, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("checksums.txt line %q is not a digest and a name", line)
		}

		// `shasum` writes the name as the glob spelled it ("./verger-…"), and a
		// user runs `shasum -c` against a downloaded file by base name.
		listed = append(listed, filepath.Base(strings.TrimSpace(name)))
	}

	return sortedUnique(listed)
}

// uploaded resolves every `actions/upload-artifact` path in this job against
// the artifacts the build steps produce: the file names a publish job has in
// its dist once it has downloaded the workflow artifacts. checksums.txt is
// added because it is generated rather than built, but it is uploaded — and
// therefore published — exactly like the tarballs.
func (j workflowJob) uploaded(t *testing.T, built []string) []string {
	t.Helper()

	available := append(slices.Clone(built), "checksums.txt")

	var uploaded []string

	for _, step := range j.Steps {
		if !strings.HasPrefix(step.Uses, "actions/upload-artifact") || step.With.Path == "" {
			continue
		}

		pattern := filepath.Base(step.With.Path)

		matched := matchNames(available, pattern)
		if len(matched) == 0 {
			// if-no-files-found: error on every upload step, so a glob that
			// matches nothing fails the release before it can be published.
			t.Fatalf("the upload step %q globs %q, which matches none of %v", step.Name, pattern, available)
		}

		uploaded = append(uploaded, matched...)
	}

	return sortedUnique(uploaded)
}

// publishedArtifacts is the asset list of the GitHub release: the file names
// `gh release upload` is handed, with its globs resolved against the dist the
// publish job actually has.
func (j workflowJob) publishedArtifacts(t *testing.T, dist []string) []string {
	t.Helper()

	step := j.step(t, "Create the GitHub release")

	var args []string

	for line := range strings.SplitSeq(step.Run, "\n") {
		fields := strings.Fields(line)

		// Find the `gh release upload <tag> <assets…>` invocation rather than
		// parsing the whole step: the step also creates the release.
		found := false

		for i := range fields {
			if strings.Join(fields[i:min(i+3, len(fields))], " ") != "gh release upload" {
				continue
			}

			args, found = fields[i+3:], true

			break
		}

		if found {
			break
		}
	}

	if len(args) == 0 {
		t.Fatal("the \"Create the GitHub release\" step no longer runs `gh release upload`")
	}

	available := sortedUnique(append(slices.Clone(dist), "checksums.txt"))

	var published []string

	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}

		published = append(published, matchNames(available, filepath.Base(arg))...)
	}

	return sortedUnique(published)
}

// matchNames is filepath.Match over a fixed name list, so a workflow glob is
// resolved by the same rules the shell resolves it by.
func matchNames(names []string, pattern string) []string {
	var matched []string

	for _, name := range names {
		if ok, err := filepath.Match(pattern, name); err == nil && ok {
			matched = append(matched, name)
		}
	}

	return matched
}

// shimPath returns a PATH in which `shasum` exists. The build job runs on a
// macOS runner, where shasum is in /usr/bin; a test run on a machine without it
// gets a one-line shim over sha256sum rather than a failure that has nothing to
// do with the workflow.
func shimPath(t *testing.T, path string) string {
	t.Helper()

	if _, err := exec.LookPath("shasum"); err == nil {
		return path
	}

	dir := t.TempDir()
	shim := filepath.Join(dir, "shasum")

	//nolint:gosec // G306: an executable shim has to be executable
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec sha256sum \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	return dir + string(os.PathListSeparator) + path
}

// withVersion resolves the `$VERSION` the steps interpolate, so a name the
// runner builds at run time has the shape the harness matches globs against.
func withVersion(name string) string {
	return strings.NewReplacer("${version}", harnessVersion, "$VERSION", harnessVersion).Replace(name)
}

func sortedUnique(names []string) []string {
	return sortedKeys(setOf(names))
}

func setOf(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}

	return set
}

func withOut(names []string, drop string) []string {
	var kept []string

	for _, name := range names {
		if name != drop {
			kept = append(kept, name)
		}
	}

	return kept
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
