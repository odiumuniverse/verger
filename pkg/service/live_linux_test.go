package service

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// execRunner is the only Runner that touches the real system. It is used by
// the live test alone, and that test refuses to run unless the environment
// says it may — so `go test ./pkg/service` on a laptop never installs a unit,
// and never would install into a real login session.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// TestLiveSystemdUserInstall runs only when VERGER_SERVICE_LIVE=1, and only on
// Linux with a real user bus. It installs a unit that runs /bin/true, reads the
// status back, and uninstalls — the whole round trip against systemd itself.
func TestLiveSystemdUserInstall(t *testing.T) {
	if os.Getenv("VERGER_SERVICE_LIVE") != "1" {
		t.Skip("set VERGER_SERVICE_LIVE=1 to install a real --user unit (Linux only)")
	}

	if goos != "linux" {
		t.Skipf("the live test is Linux-only; this platform is %s", goos)
	}

	ctx := context.Background()

	// A real home, because systemd only reads units from
	// $HOME/.config/systemd/user — a temporary home would be refused by the
	// guard and ignored by systemd. The VM is throwaway; Uninstall below
	// removes the unit again and the test fails if it is still there.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	spec := WatchSpec("/bin/true", home, home+"/.verger")
	run := execRunner{}

	path, ierr := Install(ctx, spec, run)
	err = ierr
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	t.Logf("unit written to %s", path)

	status, err := Check(ctx, home, "", run)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	t.Logf("state after install: %s (installed=%v loaded=%v)", status.State, status.Installed, status.Loaded)

	// Whatever the state, the unit must come off again.
	if _, err := Uninstall(ctx, spec, run); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	after, err := Check(ctx, home, "", run)
	if err != nil {
		t.Fatalf("check after uninstall: %v", err)
	}

	t.Logf("state after uninstall: %s", after.State)

	if after.State != StateNotInstalled {
		t.Fatalf("after uninstall the unit should be gone, got %s", after.State)
	}
}
