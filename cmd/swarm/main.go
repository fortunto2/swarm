// swarm: one binary for the CLI, the daemon (swarmd) and the per-worker MCP proxy.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
)

var profileFlag string

func load() (*config.Config, error) {
	dir, err := config.ResolveProfileDir(profileFlag)
	if err != nil {
		return nil, err
	}
	return config.Load(dir)
}

// notYet keeps the command surface stable from phase 0 while saying plainly what does not
// work yet: a stub that exits 0 would read as success.
func notYet(use, short, phase string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short + " (" + phase + ")",
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%s: not implemented yet, lands in %s (docs/plan.md)", use, phase)
		},
	}
}

func main() {
	root := &cobra.Command{
		Use:           "swarm",
		Short:         "One lead, many subscriptions: task trees to merged code",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&profileFlag, "profile", "", "profile dir holding swarm.toml (or SWARM_PROFILE)")

	root.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Load and validate the profile's swarm.toml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: profile %s, %d repo(s), %d provider(s), %d node(s), db %s\n",
				c.Profile.Name, len(c.Repos), len(c.Providers), len(c.Nodes), c.DBPath())
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Create the profile's state dir and database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			d, err := db.Open(c.DBPath())
			if err != nil {
				return err
			}
			defer d.Close()
			fmt.Fprintln(cmd.OutOrStdout(), "db ready:", c.DBPath())
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Tasks per status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			if _, err := os.Stat(c.DBPath()); err != nil {
				return fmt.Errorf("no database at %s: run `swarm init` first", c.DBPath())
			}
			d, err := db.Open(c.DBPath())
			if err != nil {
				return err
			}
			defer d.Close()
			counts, err := d.StatusCounts(context.Background(), 0)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(counts) == 0 {
				fmt.Fprintf(out, "profile %s: no tasks\n", c.Profile.Name)
				return nil
			}
			keys := make([]string, 0, len(counts))
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Fprintf(out, "profile %s\n", c.Profile.Name)
			for _, k := range keys {
				fmt.Fprintf(out, "  %-12s %d\n", k, counts[k])
			}
			return nil
		},
	})

	root.AddCommand(taskCmd(), daemonCmd(), mcpCmd(), boardCmd())
	root.AddCommand(
		notYet("run <spec.md>", "Start a run: lead builds the tree, workers execute", "phase 2"),
		notYet("logs <task>", "Event stream of a task's attempt", "phase 1"),
		notYet("stop", "SIGTERM every worker group, running tasks back to ready", "phase 5"),
		notYet("pause", "Start no new workers", "phase 5"),
		notYet("report", "Per provider: attempts, accepted, cooling, 429s", "phase 4"),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "swarm:", err)
		os.Exit(1)
	}
}
