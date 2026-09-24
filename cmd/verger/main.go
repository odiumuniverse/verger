// Command verger installs agent packages (plugins, skills, MCP servers, agents,
// commands, hooks, rules) across every supported coding agent.
package main

import (
	"fmt"
	"os"

	"github.com/odiumuniverse/verger/pkg/cli"
)

// version is stamped by the release build.
var version = "dev"

func main() {
	if err := cli.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, "verger:", err)
		os.Exit(1)
	}
}
