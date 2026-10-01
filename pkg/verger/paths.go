package verger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/odiumuniverse/verger/pkg/fsutil"
	"github.com/odiumuniverse/verger/pkg/receipt"
)

// Scope names the state tree one operation reads and writes.
type Scope string

// Scope values; they mirror receipt.ScopeUser and receipt.ScopeProject so a
// caller never has to translate between two vocabularies.
const (
	// User is the resolved verger home.
	User Scope = receipt.ScopeUser
	// Project is the repository the run was invoked from.
	Project Scope = receipt.ScopeProject
)

// Paths is the resolved state layout of one scope. Project scope lives in the
// repository: ./verger.toml, ./verger.lock and ./.verger/state (0700,
// OQ-T1.12.2).
type Paths struct {
	Scope          Scope
	Root           string // verger home root or project root
	Project        string // project root, project scope only
	SpecPath       string
	LockPath       string
	StateDir       string
	ReceiptsDir    string
	JournalPath    string
	TombstonesPath string
}

// Paths resolves the state paths of one scope. The user scope reads the
// resolved home; the project scope resolves root as the working directory, or
// as the given root when it is not empty.
func (c *Client) Paths(scope Scope, root string) (Paths, error) {
	if scope == Project {
		project := root

		if project == "" {
			wd, err := os.Getwd()
			if err != nil {
				return Paths{}, fmt.Errorf("resolve project root: %w", err)
			}

			project = wd
		}

		state := filepath.Join(project, ".verger", "state")

		return Paths{
			Scope:          Project,
			Root:           project,
			Project:        project,
			SpecPath:       filepath.Join(project, "verger.toml"),
			LockPath:       filepath.Join(project, "verger.lock"),
			StateDir:       state,
			ReceiptsDir:    filepath.Join(state, "receipts"),
			JournalPath:    filepath.Join(state, "journal.jsonl"),
			TombstonesPath: filepath.Join(state, "tombstones.json"),
		}, nil
	}

	h := c.Home()

	return Paths{
		Scope:          User,
		Root:           h.Root(),
		SpecPath:       h.SpecPath(),
		LockPath:       h.LockPath(),
		StateDir:       h.StateDir(),
		ReceiptsDir:    h.ReceiptsDir(),
		JournalPath:    h.JournalPath(),
		TombstonesPath: h.TombstonesPath(),
	}, nil
}

// Ensure creates the home and store layout for a write command (rule 1), and
// for the project scope the state dir plus its .git/info/exclude entry.
func (c *Client) Ensure(paths Paths) error {
	if err := c.Home().Ensure(); err != nil {
		return err
	}

	if err := c.Store().Ensure(); err != nil {
		return err
	}

	if paths.Scope != Project {
		return nil
	}

	if err := fsutil.EnsureDir(paths.StateDir, 0o700); err != nil {
		return err
	}

	return excludeFromGit(paths.Project)
}

// Owns reports the package that owns path, from the receipt store, and whether
// any package owns it at all. It is the ownership invariant a second front end
// filters its own pulls with.
func (c *Client) Owns(path string) (string, bool) {
	return c.OwnsIn(Paths{Scope: User, Root: c.Home().Root()}, path)
}

// OwnsIn answers the ownership invariant for one scope (DESIGN §9.3): a second
// front end filters every kind's pull through it, the project scope included,
// because a project-scoped delivery records receipts under the project root.
func (c *Client) OwnsIn(paths Paths, path string) (string, bool) {
	dir := paths.ReceiptsDir
	if dir == "" {
		dir = c.Home().ReceiptsDir()
	}

	record, _, ok := NewOwnership(dir).artifact(path)

	return record.Package, ok
}

// excludeFromGit adds the project state dir to .git/info/exclude when the
// project is a git repository (D11: generated files never become committable).
func excludeFromGit(project string) error {
	gitDir := filepath.Join(project, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return nil //nolint:nilerr // a non-git project has no exclude file
	}

	infoDir := filepath.Join(gitDir, "info")
	if err := fsutil.EnsureDir(infoDir, 0o755); err != nil {
		return fmt.Errorf("prepare %s: %w", infoDir, err)
	}

	path := filepath.Join(infoDir, "exclude")

	existing, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from the project root
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	const entry = ".verger/"

	if strings.Contains(string(existing), entry) {
		return nil
	}

	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	content += "# added by verger: generated state, never committable\n" + entry + "\n"

	return fsutil.WriteFileAtomicCAS(path, []byte(content), 0o644)
}

// Absorb moves the state of one home into another (DESIGN §9.1): the two homes
// are merged, and the source keeps nothing the target already holds under a
// different owner. It is the `verger absorb` a second front end calls when it
// takes verger over.
func Absorb(ctx context.Context, from, to *Client) (*AbsorbReport, error) {
	return moveHomes(ctx, from, to, true)
}

// Eject moves the state of one home out to a fresh empty home (DESIGN §9.1),
// the reverse of Absorb: the target must not already hold state, or the move is
// refused rather than merged.
func Eject(ctx context.Context, from, to *Client) (*AbsorbReport, error) {
	return moveHomes(ctx, from, to, false)
}

// AbsorbReport is what a home move did, as data.
type AbsorbReport struct {
	Moved    []string `json:"moved,omitempty"`   // paths taken from the source
	Skipped  []string `json:"skipped,omitempty"` // paths the target already owned
	HomeFrom string   `json:"home_from"`
	HomeTo   string   `json:"home_to"`
	Merge    bool     `json:"merge,omitempty"` // true for absorb, false for eject
	// Renamed reports that the whole home moved in one step, because the
	// target held no state to merge with (DESIGN §3.3).
	Renamed bool `json:"renamed,omitempty"`
}
