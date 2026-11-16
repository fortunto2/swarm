package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/jobs"
)

// jobsCmd: the profile's scheduled jobs ([jobs] in swarm.toml): schedule, next run, last run;
// `run <name>` starts one now through the daemon.
func jobsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "jobs",
		Short: "Scheduled jobs: their schedule, next and last run (run <name>: start one now)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(c.Jobs) == 0 {
				fmt.Fprintln(out, "no jobs in this profile ([jobs.<name>] in swarm.toml)")
				return nil
			}
			d, err := db.Open(c.DBPath())
			if err != nil {
				return err
			}
			defer d.Close()
			now := time.Now()
			for _, name := range slices.Sorted(maps.Keys(c.Jobs)) {
				j := c.Jobs[name]
				s, _ := jobs.Parse(j.Cron)
				line := fmt.Sprintf("%-22s %-18s next %s", name, j.Cron, s.Next(now).Format("01-02 15:04Z"))
				if runs, err := d.JobRuns(context.Background(), name, 1); err == nil && len(runs) == 1 {
					r := runs[0]
					ago := now.Sub(time.Unix(r.StartedAt, 0)).Round(time.Minute)
					switch {
					case r.Skipped != "":
						line += fmt.Sprintf("; last %s ago skipped (%s)", ago, r.Skipped)
					case r.EndedAt == 0:
						line += fmt.Sprintf("; running for %s", ago)
					case r.Exit != nil && *r.Exit == 0:
						line += fmt.Sprintf("; last %s ago ok", ago)
					case r.Exit != nil:
						last := strings.TrimSpace(r.Tail)
						if i := strings.LastIndex(last, "\n"); i >= 0 {
							last = last[i+1:]
						}
						line += fmt.Sprintf("; last %s ago FAILED exit %d: %.120s", ago, *r.Exit, last)
					}
				} else {
					line += "; never ran"
				}
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
	c.AddCommand(&cobra.Command{
		Use:   "run <name>",
		Short: "Start a job now, whatever its schedule says (needs the daemon running)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			cl, err := board.Dial(dispatch.SocketPath(c.StatePath()))
			if err != nil {
				return fmt.Errorf("start the daemon first (swarm daemon): %w", err)
			}
			defer cl.Close()
			tok, err := board.OperatorToken(c.StatePath())
			if err != nil {
				return err
			}
			res, err := cl.Do("swarm_run_job", tok, map[string]string{"job": a[0]})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(res))
			return nil
		},
	})
	return c
}

// printJobs: one status line for the scheduled jobs, naming those whose newest run failed.
func printJobs(out io.Writer, c *config.Config, d *db.DB) {
	if len(c.Jobs) == 0 {
		return
	}
	runs, err := d.LatestJobRuns(context.Background())
	if err != nil {
		return
	}
	var failing []string
	for _, r := range runs {
		if _, ok := c.Jobs[r.Job]; ok && r.Exit != nil && *r.Exit != 0 {
			failing = append(failing, fmt.Sprintf("%s exit %d %s ago", r.Job, *r.Exit,
				time.Since(time.Unix(r.StartedAt, 0)).Round(time.Minute)))
		}
	}
	line := fmt.Sprintf("  jobs: %d", len(c.Jobs))
	if len(failing) > 0 {
		line += ", last run failed: " + strings.Join(failing, "; ") + " (swarm jobs)"
	}
	fmt.Fprintln(out, line)
}
