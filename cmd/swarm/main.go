// swarm: one binary for the CLI, the daemon (swarmd) and the per-worker MCP proxy.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"

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
			counts, err := d.StatusCounts(context.Background())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(counts) == 0 {
				fmt.Fprintf(out, "profile %s: no tasks\n", c.Profile.Name)
				return nil
			}
			fmt.Fprintf(out, "profile %s\n", c.Profile.Name)
			for _, k := range slices.Sorted(maps.Keys(counts)) {
				fmt.Fprintf(out, "  %-12s %d\n", k, counts[k])
			}
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of swarm.toml (editor completion, CI validation)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := config.Schema()
			if err != nil {
				return err
			}
			b, err := json.MarshalIndent(s, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return err
		},
	})
	root.AddCommand(taskCmd(), daemonCmd(), mcpCmd(), boardCmd(), gateCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "swarm:", err)
		os.Exit(1)
	}
}
