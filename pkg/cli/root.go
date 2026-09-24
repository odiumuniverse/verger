// Package cli implements the verger cobra command tree.
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCmd builds the complete verger command tree.
func NewRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "verger",
		Short:         "Install agent packages everywhere",
		Long:          "verger installs plugins, skills, MCP servers, agents, commands and hooks across every supported coding agent.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The interactive TUI replaces this help screen in a later phase.
			return cmd.Help()
		},
	}
	root.AddCommand(newVersionCmd(version))

	return root
}

// Execute runs the verger command tree with the release version.
func Execute(version string) error {
	return NewRootCmd(version).Execute()
}
