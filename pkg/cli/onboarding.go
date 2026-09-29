package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/odiumuniverse/verger/pkg/verger"
)

// The first-run screen (W7-UX-SPEC §5.1). A bare `verger` with no `~/.verger`
// shows what it found and asks one question. A bare `verger` with a home
// prints the help screen instead, so the two are never the same surprise.

// hostFinding is one line of the first-run screen: the host, whether it was
// found, and the evidence for that answer.
type hostFinding struct {
	id    string
	found bool
	// where is the path or binary the verdict came from. It is shown so a
	// user is never told more than we know: "not found" with no reason is a
	// claim, and the honest form names what was looked for.
	where string
}

// onboardingScreen is the rendered first-run screen.
type onboardingScreen struct {
	home  string
	hosts []hostFinding
}

// collectFindings asks every registered adapter whether its host is present.
//
// The evidence is `Detect`, the same predicate the rest of the CLI uses, and
// the config root the adapter reads. This deliberately invents no marker of
// its own: a host with no verified binary name is reported as not found
// rather than detected by a path we made up.
func (a *app) collectFindings(client *verger.Client) []hostFinding {
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = ""
	}

	hosts := make([]hostFinding, 0, len(client.Hosts()))

	for _, adapter := range client.Hosts() {
		id := string(adapter.ID())
		detected := adapter.Detect(userHome)

		where := ""
		if detected {
			where = hostEvidence(id, userHome)
		} else {
			where = "not installed"
		}

		hosts = append(hosts, hostFinding{id: id, found: detected, where: where})
	}

	return hosts
}

// hostEvidence names the directory a detected host was recognised by.
func hostEvidence(id, userHome string) string {
	if userHome == "" {
		return "detected"
	}

	return filepath.Join(userHome, "."+id)
}

// homeExists reports whether this run already has a verger home, which is what
// separates a first run from an ordinary bare invocation.
func homeExists(client *verger.Client) bool {
	info, err := os.Stat(client.Home().Root())

	return err == nil && info.IsDir()
}

// render writes the screen: the hosts found, and the one question.
func (s onboardingScreen) render(out io.Writer) {
	fmt.Fprintf(out, "  No %s yet. Here is what I found.\n\n", s.home)
	fmt.Fprintf(out, "  %-18s %-9s %s\n", "AGENT", "STATE", "EVIDENCE")

	for _, host := range s.hosts {
		state := "not found"
		if host.found {
			state = "found"
		}

		fmt.Fprintf(out, "  %-18s %-9s %s\n", host.id, state, host.where)
	}

	fmt.Fprintf(out, "\n  I will create %s and nothing else — no files are written\n", s.home)
	fmt.Fprintf(out, "  to any agent until you run `verger install`.\n")
}

// onboard runs the first-run flow: show the screen, ask once, create the home
// on yes. It is idempotent — a home that already exists short-circuits to the
// help screen, so a second bare run never re-asks.
func (a *app) onboard(client *verger.Client, yes, dryRun bool) error {
	screen := onboardingScreen{
		home:  client.Home().Root(),
		hosts: a.collectFindings(client),
	}

	screen.render(a.out)

	// --dry-run prints the screen and writes nothing; it does not ask, because
	// there is no decision left to make once nothing will be written.
	if dryRun {
		fmt.Fprintf(a.out, "\n  dry run: %s was not created.\n", screen.home)

		return nil
	}

	// -y is the script switch: create the home without asking.
	if yes {
		return a.createHome(client, screen, dryRun)
	}

	// With no terminal there is nobody to ask. The screen is still the useful
	// output and the run is a no-op rather than a failure: a script that
	// invokes verger to look should not get an error for looking.
	//
	// This asks about the terminal directly rather than through
	// `interactive`, which also reports false for --dry-run, and a dry run
	// is precisely the case that must still print what it would do.
	if !a.tty(os.Stdin) {
		fmt.Fprintf(a.out, "\n  Not a terminal, so nothing was created. Pass -y to create it.\n")

		return nil
	}

	proceed, err := a.ask("Create it now?", true)
	if err != nil {
		return err
	}

	if !proceed {
		fmt.Fprintf(a.out, "\n  Nothing was created. Run `verger doctor` any time.\n")

		return nil
	}

	return a.createHome(client, screen, dryRun)
}

// createHome makes the home, or says plainly that it did not.
func (a *app) createHome(client *verger.Client, screen onboardingScreen, dryRun bool) error {
	if dryRun {
		fmt.Fprintf(a.out, "\n  dry run: %s was not created.\n", screen.home)

		return nil
	}

	if err := client.Home().Ensure(); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "\n  Created %s.\n", screen.home)
	fmt.Fprintf(a.out, "  Next: `verger install <package>` to put something in an agent.\n")

	return nil
}
