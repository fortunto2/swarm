package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// revFile: the daemon writes the commit it was built from here, so status can tell a daemon
// still running an older build after a `go install` from one that picked the new one up.
const revFile = "daemon.rev"

// revision: the commit this binary was built from, "+dirty" when the tree had changes; ""
// when the build carries no VCS stamp (go run, a build outside a checkout).
func revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision":
			rev = s.Value
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = "+dirty"
		}
	}
	if rev == "" {
		return ""
	}
	return rev[:min(7, len(rev))] + dirty
}

// staleDaemon: the status line when the running daemon was built from another commit than
// this binary; "" when they match or either side is unknown.
func staleDaemon(stateDir, self string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, revFile))
	ran := strings.TrimSpace(string(b))
	if err != nil || ran == "" || self == "" || ran == self {
		return ""
	}
	return fmt.Sprintf("daemon runs %s, this binary is %s: restart the daemon to pick it up", ran, self)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "The commit this binary was built from",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			rev := revision()
			if rev == "" {
				rev = "unknown (built without a VCS stamp)"
			}
			fmt.Fprintln(cmd.OutOrStdout(), "swarm", rev)
		},
	}
}
