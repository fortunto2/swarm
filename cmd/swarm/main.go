// swarm: one binary for the CLI, the daemon (swarmd) and the per-worker MCP proxy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/gitx"
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
			if c.Node != nil { // a node: its repos are copies, their mains are the coordinator's business
				fmt.Fprintf(cmd.OutOrStdout(), "ok: node %s (profile %s), %d repo(s), coordinator: %s\n",
					c.Node.Name, c.Profile.Name, len(c.Repos), strings.Join(c.Node.Coordinator, " "))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: profile %s, %d repo(s), %d provider(s), %d node(s), db %s\n",
				c.Profile.Name, len(c.Repos), len(c.Providers), len(c.Nodes), c.DBPath())
			for _, name := range slices.Sorted(maps.Keys(c.Repos)) {
				if w := staleMain(name, c.Repos[name]); w != "" {
					fmt.Fprintln(cmd.OutOrStdout(), w)
				}
			}
			for _, id := range slices.Sorted(maps.Keys(c.Providers)) {
				if c.NodeOf(id) == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "warn: provider %s is in no node's accounts while %s lists its own: it never runs\n", id, c.LocalNode())
				}
			}
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
			if c.Node != nil {
				return nodeStatus(cmd.OutOrStdout(), c)
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
				printNodes(out, c, d) // an empty queue is when one checks that the machines are there
				return nil
			}
			fmt.Fprintf(out, "profile %s\n", c.Profile.Name)
			for _, k := range slices.Sorted(maps.Keys(counts)) {
				fmt.Fprintf(out, "  %-12s %d\n", k, counts[k])
			}
			if !dispatch.Running(c.StatePath()) {
				// a hold recorded by a daemon that is gone says nothing about now
				fmt.Fprintln(out, "  daemon not running: nothing starts until `swarm daemon` runs")
				printNodes(out, c, d)
				return nil
			}
			if s := staleDaemon(c.StatePath(), revision()); s != "" {
				fmt.Fprintln(out, "  "+s)
			}
			if _, err := os.Stat(dispatch.DrainFlag(c.StatePath())); err == nil {
				fmt.Fprintln(out, "  draining: finishing what is in flight, then exiting for a restart (swarm drain --cancel)")
			}
			if why, since, held := d.Held(context.Background()); held && c.Limits.Admit != "" {
				fmt.Fprintf(out, "  held since %s: %s (limits.admit; nothing new starts)\n",
					time.Unix(since, 0).UTC().Format("01-02 15:04Z"), why)
			}
			printNodes(out, c, d)
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
	root.AddCommand(&cobra.Command{
		Use:   "providers",
		Short: "The provider registry: roles, priority, slots, state, spend today against budget",
		Args:  cobra.NoArgs,
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
			views, err := dispatch.ProviderViews(context.Background(), c, d)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-14s %-8s %-7s %-10s %-22s %4s %7s %-9s %s\n", "PROVIDER", "NODE", "KIND", "FAMILY", "ROLES", "PRIO", "SLOTS", "STATE", "TODAY")
			for _, v := range views {
				budget := "no cap"
				if v.BudgetDay > 0 {
					budget = fmt.Sprintf("of $%.2f", v.BudgetDay)
				}
				state := v.State
				if v.Until != "" {
					state += " until " + v.Until
				}
				fmt.Fprintf(out, "%-14s %-8s %-7s %-10s %-22s %4d %3d/%-3d %-9s $%.2f %s\n", v.ID, v.Node, v.Kind, v.Family,
					strings.Join(v.Roles, ","), v.Priority, v.Running, v.Slots, state, v.SpentToday, budget)
				if v.Why != "" {
					fmt.Fprintf(out, "%-14s %s\n", "", v.Why)
				}
			}
			return nil
		},
	})
	root.AddCommand(reportCmd())
	root.AddCommand(taskCmd(), daemonCmd(), mcpCmd(), boardCmd(), gateCmd(), selftestCmd(), versionCmd(), nodeCmd(), relayCmd(), drainCmd(), jobsCmd())

	if err := root.Execute(); err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, "swarm:", err)
		}
		os.Exit(1)
	}
}

// errReported: the command already printed why it failed; exit 1 without saying it twice.
var errReported = errors.New("failure reported")

// staleMain: a warning when a repo's main_branch is a local branch behind its upstream. swarm
// never fetches and branches every task from what main_branch names, so a checkout nobody
// pulls (people work in other worktrees) hands every worker an old base.
func staleMain(name string, r config.Repo) string {
	if strings.HasPrefix(r.MainBranch, "refs/") {
		return ""
	}
	ctx := context.Background()
	path := config.Expand(r.Path)
	up, err := gitx.Run(ctx, path, "rev-parse", "--abbrev-ref", r.MainBranch+"@{upstream}")
	if err != nil || up == "" {
		return "" // no upstream: local main is the truth
	}
	n, err := gitx.Run(ctx, path, "rev-list", "--count", r.MainBranch+".."+up)
	if err != nil || n == "0" {
		return ""
	}
	return fmt.Sprintf("warn: repos.%s: %s is %s commit(s) behind %s, and every task branches from it (swarm never fetches). "+
		"Use main_branch = \"refs/remotes/%s\" with merge = \"branch\", or keep %s pulled.", name, r.MainBranch, n, up, up, r.MainBranch)
}
