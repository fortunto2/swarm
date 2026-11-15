package main

// selftest: the whole loop (task → worker → gate → ff merge) on one of the profile's real
// providers, against a throwaway repo in a temp dir. No repo of the profile is touched, and
// the profile's own daemon may keep running: the temp profile has its own state, database,
// lock and socket.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/provider"
)

const (
	selftestTitle      = "selftest: write 42"
	selftestSpec       = "create answer.txt whose only line is 42"
	selftestAcceptance = "the gate reads the file"
	selftestFile       = "answer.txt"
	localNode          = "local" // the node a daemon started here runs as (dispatch.New)
)

func selftestCmd() *cobra.Command {
	var want string
	var timeout time.Duration
	var keep bool
	c := &cobra.Command{
		Use:   "selftest",
		Short: "Prove the loop (task, worker, gate, ff merge) on a real provider against a throwaway repo",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := load()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			out := cmd.OutOrStdout()
			// past this point every outcome is one selftest line, a setup failure included
			setupFailed := func(err error) error {
				fmt.Fprintln(out, "selftest FAILED: setup: "+oneLine(err.Error()))
				return errReported
			}
			pid, err := selftestProvider(ctx, src, want)
			if err != nil {
				return setupFailed(err)
			}
			dir, err := os.MkdirTemp("", "swarm-selftest-")
			if err != nil {
				return setupFailed(err)
			}
			// the real path: on macOS the temp dir is behind a symlink, and sandbox and deny
			// rules compare paths as written
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				dir = real
			}
			defer func() {
				_ = os.Remove(dispatch.SocketPath(filepath.Join(dir, "state")))
				if keep {
					fmt.Fprintln(out, "kept:", dir)
					return
				}
				_ = os.RemoveAll(dir)
			}()
			line, ok, err := selftest(ctx, src, pid, dir, timeout)
			if err != nil {
				return setupFailed(err)
			}
			fmt.Fprintln(out, line)
			if !ok {
				return errReported
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&want, "provider", "", "provider to test (default: the first worker by priority that is enabled)")
	f.DurationVar(&timeout, "timeout", 10*time.Minute, "wall clock for the whole loop")
	f.BoolVar(&keep, "keep", false, "keep the temp dir (repo, profile, state, daemon.log) and print its path")
	return c
}

// selftest builds the throwaway repo and the temp profile in dir, adds the one task, runs the
// daemon until idle and judges. An error is a setup failure; a failed loop is a line.
func selftest(ctx context.Context, src *config.Config, pid, dir string, timeout time.Duration) (line string, ok bool, err error) {
	repo := filepath.Join(dir, "repo")
	if err := throwawayRepo(ctx, repo); err != nil {
		return "", false, fmt.Errorf("selftest repo: %w", err)
	}
	st, err := src.Selftest(pid, dir, repo, timeout)
	if err != nil {
		return "", false, err
	}
	var buf bytes.Buffer
	if err := st.Encode(&buf); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "swarm.toml"), buf.Bytes(), 0o600); err != nil {
		return "", false, err
	}
	c, err := config.Load(dir) // the daemon runs what is on disk, as for any profile
	if err != nil {
		return "", false, fmt.Errorf("selftest profile: %w", err)
	}
	id, err := addSelftestTask(ctx, c, selftestTier(c.Providers[pid]))
	if err != nil {
		return "", false, err
	}
	logf, err := os.Create(filepath.Join(dir, "daemon.log"))
	if err != nil {
		return "", false, err
	}
	defer logf.Close()
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	runErr := runDaemon(rctx, c, true, logf)
	took := time.Since(start).Round(100 * time.Millisecond)
	stopped := rctx.Err()
	cancel()
	why := "" // the loop did not end on its own
	switch {
	case runErr != nil:
		why = "daemon: " + runErr.Error()
	case errors.Is(stopped, context.DeadlineExceeded):
		why = fmt.Sprintf("no result within %s (--timeout)", timeout)
	case stopped != nil:
		why = "interrupted"
	}
	line, ok = judge(context.Background(), c, id, pid, took, why) // ctx may be the one interrupted
	return line, ok, nil
}

// throwawayRepo: branch main, one commit with a README. The identity is given here, so a
// machine without a git identity can run it, and the operator's signing setup is not asked.
func throwawayRepo(ctx context.Context, path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, path, "init", "--quiet", "--initial-branch=main"); err != nil {
		return err
	}
	readme := "# swarm selftest\n\nA throwaway repo: one task writes answer.txt, the gate reads it.\n"
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, path, "add", "README.md"); err != nil {
		return err
	}
	_, err := gitx.Run(ctx, path, "-c", "user.name=swarm selftest", "-c", "user.email=selftest@example.com",
		"-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "selftest: base")
	return err
}

func addSelftestTask(ctx context.Context, c *config.Config, tier string) (int64, error) {
	d, err := db.Open(c.DBPath())
	if err != nil {
		return 0, err
	}
	defer d.Close()
	base, err := gitx.Head(ctx, c.Repos[config.SelftestRepo].Path, "refs/heads/main")
	if err != nil {
		return 0, err
	}
	run, err := d.CreateRun(ctx, "task:"+selftestTitle, "", base)
	if err != nil {
		return 0, err
	}
	return d.AddTask(ctx, db.NewTask{RunID: run, Repo: config.SelftestRepo, Title: selftestTitle, Spec: selftestSpec,
		Acceptance: selftestAcceptance, Tier: tier, Files: []string{selftestFile}})
}

// judge: pass is the task merged, main's last commit by this provider's worker, and the
// file on main holding 42. Anything else is a FAILED line: why the loop stopped early (if
// it did), then the reason swarm recorded.
func judge(ctx context.Context, c *config.Config, id int64, pid string, took time.Duration, why string) (string, bool) {
	d, err := db.Open(c.DBPath())
	if err != nil {
		return "selftest FAILED: unknown: " + err.Error(), false
	}
	defer d.Close()
	t, err := d.GetTask(ctx, id)
	if err != nil {
		return "selftest FAILED: unknown: " + err.Error(), false
	}
	failed := func(reason string) (string, bool) {
		return fmt.Sprintf("selftest FAILED: %s: %s", t.Status, reason), false
	}
	if t.Status != db.Merged {
		reason := failReason(ctx, d, t, pid)
		switch {
		case why != "" && reason != "":
			reason = why + "; last recorded: " + reason
		case why != "":
			reason = why
		case reason == "":
			reason = "nothing recorded; run again with --keep and read daemon.log"
		}
		return failed(reason)
	}
	repo := c.Repos[config.SelftestRepo].Path
	if author, err := gitx.Run(ctx, repo, "log", "-1", "--format=%an", "refs/heads/main"); err != nil || !strings.HasPrefix(author, "swarm-"+pid+"-T") {
		return failed(fmt.Sprintf("main's last commit is by %q, not swarm-%s-T…", author, pid))
	}
	if answer, err := gitx.Run(ctx, repo, "show", "refs/heads/main:"+selftestFile); err != nil || answer != "42" {
		return failed(fmt.Sprintf("%s on main holds %q, not 42", selftestFile, answer))
	}
	spent, _ := d.SpentSinceByProvider(ctx, 0)
	return fmt.Sprintf("selftest ok: %s merged in %s, $%.2f", pid, took, spent[pid]), true
}

// failReason: what swarm recorded about the task: its last verdict note (gate output, a
// refusal, a merge error, the worker's question), else its last failure event (a crash), else
// the provider's state (not logged in). A worker's plan or progress note is not a reason: it
// would hide the crash that came after it. "" when there is nothing.
func failReason(ctx context.Context, d *db.DB, t db.Task, pid string) string {
	notes, _ := d.Notes(ctx, t.ID, "", 50)
	for _, n := range notes {
		if n.Kind == "review" || n.Kind == "question" {
			return oneLine(n.Body)
		}
	}
	var data string
	if d.QueryRowContext(ctx, `SELECT data FROM events WHERE task_id=? AND type IN ('infra_fail','model_fail')
		ORDER BY id DESC LIMIT 1`, t.ID).Scan(&data) == nil {
		var e struct{ Outcome, Detail string }
		if json.Unmarshal([]byte(data), &e) == nil {
			return oneLine(e.Outcome + ": " + e.Detail)
		}
	}
	if states, err := d.ProviderStates(ctx); err == nil {
		if st := states[pid]; st.State == "disabled" || st.State == "cooling" {
			return oneLine(fmt.Sprintf("provider %s %s: %s", pid, st.State, st.LastError))
		}
	}
	return ""
}

// oneLine: a recorded reason as one printable line, its lines joined with "; ".
func oneLine(s string) string {
	var parts []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	out := strings.Join(parts, "; ")
	if r := []rune(out); len(r) > 400 {
		out = string(r[:400]) + "…"
	}
	return out
}

// selftestProvider: --provider when given, else the first worker by priority that this node
// runs and the profile's own registry has not disabled or cooled.
func selftestProvider(ctx context.Context, c *config.Config, want string) (string, error) {
	usable := func(id string) error {
		p := c.Providers[id]
		if !p.Can("worker") {
			return fmt.Errorf("provider %s is not a worker (roles %v)", id, p.Roles)
		}
		if _, err := provider.New(id, p, c.Profile); err != nil {
			return err
		}
		if n, ok := c.Nodes[localNode]; ok && len(n.Accounts) > 0 && !slices.Contains(n.Accounts, id) {
			return fmt.Errorf("provider %s is not in nodes.%s.accounts", id, localNode)
		}
		return nil
	}
	ids := slices.Sorted(maps.Keys(c.Providers))
	if want != "" {
		if _, ok := c.Providers[want]; !ok {
			return "", fmt.Errorf("profile has no provider %q (it has %s)", want, strings.Join(ids, ", "))
		}
		return want, usable(want)
	}
	sort.SliceStable(ids, func(i, j int) bool { return c.Providers[ids[i]].Priority < c.Providers[ids[j]].Priority })
	states := profileStates(ctx, c)
	var why []string
	for _, id := range ids {
		if err := usable(id); err != nil {
			why = append(why, err.Error())
			continue
		}
		if st := states[id]; st.State == "disabled" || (st.State == "cooling" && st.CoolingUntil > time.Now().Unix()) {
			why = append(why, fmt.Sprintf("provider %s is %s: %s", id, st.State, st.LastError))
			continue
		}
		return id, nil
	}
	return "", fmt.Errorf("no provider to test: %s", strings.Join(why, "; "))
}

// profileStates: the profile's own registry, read only if its database exists (selftest
// never creates state in the profile it tests).
func profileStates(ctx context.Context, c *config.Config) map[string]db.ProviderState {
	if _, err := os.Stat(c.DBPath()); err != nil {
		return nil
	}
	d, err := db.Open(c.DBPath())
	if err != nil {
		return nil
	}
	defer d.Close()
	states, _ := d.ProviderStates(ctx)
	return states
}

// selftestTier: the cheapest tier the provider takes.
func selftestTier(p config.Provider) string {
	for _, t := range []string{"B", "A", "S"} {
		if len(p.Tiers) == 0 || slices.Contains(p.Tiers, t) {
			return t
		}
	}
	return "B"
}
