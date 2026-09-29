// Command verger installs agent packages (plugins, skills, MCP servers, agents,
// commands, hooks, rules) across every supported coding agent.
package main

import (
	"fmt"
	"os"

	"github.com/odiumuniverse/verger/pkg/cli"
	"github.com/odiumuniverse/verger/pkg/exitcode"
	"github.com/odiumuniverse/verger/pkg/watchbind"
)

// version is stamped by the release build.
var version = "dev"

func main() {
	// The facade declares a watch seam so it never imports the engine; the
	// binary is the one place that binds them. Without this, `verger watch`
	// answered "not available" in verger's own build.
	watchbind.Install()

	if err := cli.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, "verger:", err)
		// The status is a class, not a boolean: a conflict the user must
		// settle is not a crash, and a script that only ever sees 1 cannot
		// tell them apart. See pkg/exitcode for the table.
		os.Exit(exitcode.Classify(err))
	}
}
