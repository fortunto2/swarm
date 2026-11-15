package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fortunto2/swarm/internal/db"
	"github.com/spf13/cobra"
)

// The read side of the lead's MCP tools, for a terminal or an ssh session: they open the
// database directly (WAL readers never block the daemon), so they work while it is down.

func withDB(f func(*db.DB) error) error {
	c, err := load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	return f(d)
}

func taskListCmd() *cobra.Command {
	var status string
	var n int
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Short: "Newest tasks, optionally one status (as swarm_list_tasks)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDB(func(d *db.DB) error {
				rows, err := d.RecentTasks(context.Background(), status, n)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if asJSON {
					b, _ := json.MarshalIndent(rows, "", "  ")
					fmt.Fprintln(out, string(b))
					return nil
				}
				for _, r := range rows {
					where := r.Node
					if where == "" {
						where = "-"
					}
					line := fmt.Sprintf("T%-4d %-10s %-16s %-8s %d att  %s", r.ID, r.Status, r.Repo, where, r.Attempts, r.Title)
					if r.RunsOn != "" {
						line += "  [runs_on " + r.RunsOn + "]"
					}
					fmt.Fprintln(out, line)
				}
				return nil
			})
		},
	}
	c.Flags().StringVar(&status, "status", "", "only this status")
	c.Flags().IntVarP(&n, "n", "n", 50, "at most this many")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

func taskNotesCmd() *cobra.Command {
	var n int
	c := &cobra.Command{
		Use:   "notes <id>",
		Short: "Why a task is where it is: gate failures, plans, questions, newest first (as swarm_task_notes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "T"), 10, 64)
			if err != nil {
				return fmt.Errorf("task id: %w", err)
			}
			return withDB(func(d *db.DB) error {
				notes, err := d.Notes(context.Background(), id, "", n)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				for _, x := range notes {
					fmt.Fprintf(out, "-- %s %s (%s)\n%s\n", time.Unix(x.At, 0).UTC().Format("01-02 15:04Z"), x.Kind, x.Author, x.Body)
				}
				if len(notes) == 0 {
					fmt.Fprintf(out, "T%d: no notes\n", id)
				}
				return nil
			})
		},
	}
	c.Flags().IntVarP(&n, "n", "n", 10, "at most this many")
	return c
}
