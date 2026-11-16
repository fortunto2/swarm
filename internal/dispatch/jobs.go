package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/jobs"
	"github.com/fortunto2/swarm/internal/provider"
)

// jobState: which scheduled jobs are running now. Started from the loop and from the board
// (`swarm jobs run`), ended by their goroutine, so it has its own lock.
type jobState struct {
	mu      sync.Mutex
	running map[string]bool
	remote  map[string]remoteJob // running on a node: ended by its report, or by the guard
}

// remoteJob: a run ordered to a node; past deadline with no report it is closed as lost.
type remoteJob struct {
	run      int64
	node     string
	deadline time.Time
}

func (j *jobState) setRemote(name string, r remoteJob) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.remote == nil {
		j.remote = map[string]remoteJob{}
	}
	j.remote[name] = r
}

// takeRemote: the remote run of a job, removed, if it is the one named (run 0: any past now).
func (j *jobState) takeRemote(name string, run int64, now time.Time) (remoteJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.remote[name]
	if !ok || (run != 0 && r.run != run) || (run == 0 && now.Before(r.deadline)) {
		return remoteJob{}, false
	}
	delete(j.remote, name)
	delete(j.running, name)
	return r, true
}

func (j *jobState) set(name string, on bool) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running == nil {
		j.running = map[string]bool{}
	}
	if on && j.running[name] {
		return false // one run of a job at a time
	}
	if on {
		j.running[name] = true
	} else {
		delete(j.running, name)
	}
	return true
}

func (j *jobState) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.running)
}

// jobTimeout: a job's own, else 30 minutes.
func jobTimeout(j config.Job) time.Duration {
	if j.Timeout.Duration > 0 {
		return j.Timeout.Duration
	}
	return 30 * time.Minute
}

// jobsTick starts every job whose latest scheduled minute it has not run for. A job seen for
// the first time is registered at that minute and waits for the next one; a minute missed
// while the daemon was down runs once when it is back (the latest one only). A spare job on a
// host that is not admitting is skipped for that minute, and the skip is recorded.
func (d *Daemon) jobsTick(ctx context.Context, now time.Time) {
	if len(d.Cfg.Jobs) == 0 {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(d.Cfg.Jobs)) { // a node that never reported
		if r, ok := d.jobs.takeRemote(name, 0, now); ok {
			d.endJob(ctx, name, d.Cfg.Jobs[name], r.run, -1, "[no report from node "+r.node+" by the job's timeout: lost]", 0)
		}
	}
	if d.draining.Load() {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(d.Cfg.Jobs)) {
		j := d.Cfg.Jobs[name]
		s, err := jobs.Parse(j.Cron) // validated at load
		if err != nil {
			continue
		}
		prev := s.Prev(now)
		if prev.IsZero() {
			continue
		}
		last, known, err := d.DB.JobDue(ctx, name)
		if err != nil {
			continue
		}
		if !known {
			_ = d.DB.SetJobDue(ctx, name, prev.Unix())
			continue
		}
		if prev.Unix() <= last {
			continue
		}
		if j.Node != "" {
			// its node runs it: away (or too old for job orders), the minute waits for it
			online, admitting := d.hub.jobNode(j.Node)
			if !online {
				continue
			}
			if j.Spare && !admitting {
				_ = d.DB.SetJobDue(ctx, name, prev.Unix())
				_, _ = d.DB.StartJobRun(ctx, name, prev.Unix(), "held: node "+j.Node+" is not admitting")
				continue
			}
		} else if j.Spare && d.Cfg.Limits.Admit != "" {
			if !d.admit.checked {
				continue // the first answer of limits.admit is on its way: decide then, not skip blind
			}
			if !d.admit.ok {
				_ = d.DB.SetJobDue(ctx, name, prev.Unix())
				_, _ = d.DB.StartJobRun(ctx, name, prev.Unix(), "held: "+d.admit.why)
				d.Log.Info("job skipped for the host", "job", name, "why", d.admit.why)
				continue
			}
		}
		// the minute is taken before the start, as cron does: a run that fails to record its
		// minute must not run again for it, and a job still running skips this one
		if err := d.DB.SetJobDue(ctx, name, prev.Unix()); err != nil {
			continue
		}
		if _, err := d.startJob(ctx, name, prev.Unix()); errors.Is(err, errJobRunning) {
			_, _ = d.DB.StartJobRun(ctx, name, prev.Unix(), "skipped: the previous run is still going")
		}
	}
}

var errJobRunning = errors.New("the job is running")

// startJob runs a job now, off the loop: its run row, its exit and output tail, a notification
// by its notify setting. due is the scheduled minute it runs for (0: started by hand).
func (d *Daemon) startJob(ctx context.Context, name string, due int64) (int64, error) {
	j, ok := d.Cfg.Jobs[name]
	if !ok {
		return 0, fmt.Errorf("no job %q", name)
	}
	if j.Node != "" {
		return d.startRemoteJob(ctx, name, j, due)
	}
	d.notifyMu.Lock()
	if d.closing {
		d.notifyMu.Unlock()
		return 0, errors.New("the daemon is stopping")
	}
	if !d.jobs.set(name, true) {
		d.notifyMu.Unlock()
		return 0, errJobRunning
	}
	d.bg.Add(1)
	d.notifyMu.Unlock()
	id, err := d.DB.StartJobRun(ctx, name, due, "")
	if err != nil {
		d.jobs.set(name, false)
		d.bg.Done()
		return 0, err
	}
	dir := d.Cfg.Dir
	if r, ok := d.Cfg.Repos[j.Repo]; ok {
		dir = config.Expand(r.Path)
	}
	go func() {
		defer d.bg.Done()
		defer d.jobs.set(name, false)
		if j.Prompt != "" {
			run, err := provider.Agent(j.Provider, d.Cfg.Providers[j.Provider], d.Cfg.Profile, j.AllowedTools)
			exit, out, cost := AgentRun(d.prepCtx, run, err, j.Prompt, dir, jobTimeout(j))
			d.endJob(context.WithoutCancel(ctx), name, j, id, exit, out, cost)
			return
		}
		// the operator's command, as hooks are: the daemon's environment, plus where it runs from
		env := append(os.Environ(), "SWARM_JOB="+name, "SWARM_PROFILE="+d.Cfg.Dir)
		exit, out := jobs.Exec(d.prepCtx, j.Run, dir, env, jobTimeout(j)) // the daemon stopping stops it too
		d.endJob(context.WithoutCancel(ctx), name, j, id, exit, out, 0)
	}()
	return id, nil
}

// startRemoteJob orders a job to its node; the node's report (or the guard, past the job's
// timeout and five minutes more) ends the run.
func (d *Daemon) startRemoteJob(ctx context.Context, name string, j config.Job, due int64) (int64, error) {
	if online, _ := d.hub.jobNode(j.Node); !online {
		return 0, fmt.Errorf("node %s is not connected (or too old for job orders)", j.Node)
	}
	if !d.jobs.set(name, true) {
		return 0, errJobRunning
	}
	id, err := d.DB.StartJobRun(ctx, name, due, "")
	if err != nil {
		d.jobs.set(name, false)
		return 0, err
	}
	d.jobs.setRemote(name, remoteJob{run: id, node: j.Node, deadline: time.Now().Add(jobTimeout(j) + 5*time.Minute)})
	d.hub.order(j.Node, board.NodeCmd{Kind: "job", Job: name, Command: j.Run, Repo: j.Repo, RunID: id,
		TimeoutSec: int(jobTimeout(j).Seconds()), Prompt: j.Prompt, Provider: j.Provider, AllowedTools: j.AllowedTools})
	d.DB.Event(ctx, "swarmd", 0, "job_ordered", map[string]any{"job": name, "run": id, "node": j.Node})
	return id, nil
}

// remoteJobDone: a node reported a job run's end.
func (d *Daemon) remoteJobDone(ctx context.Context, node string, r board.NodeReport) {
	rj, ok := d.jobs.takeRemote(r.Job, r.RunID, time.Now())
	if !ok || rj.node != node {
		return // not ours, or already closed as lost
	}
	d.endJob(ctx, r.Job, d.Cfg.Jobs[r.Job], r.RunID, r.Exit, r.Tail, r.Cost)
}

// endJob records a run's end, wherever it ran, and tells the operator by the job's notify.
func (d *Daemon) endJob(ctx context.Context, name string, j config.Job, id int64, exit int, out string, cost float64) {
	text := d.redact.Apply(out)
	_ = d.DB.EndJobRun(ctx, id, exit, text, cost)
	d.DB.Event(ctx, "swarmd", 0, "job_done", map[string]any{"job": name, "run": id, "exit": exit})
	d.Log.Info("job done", "job", name, "exit", exit)
	notify := j.Notify
	if notify == "" {
		notify = "fail"
	}
	if notify == "always" || (notify == "fail" && exit != 0) {
		last := strings.TrimSpace(text)
		if i := strings.LastIndex(last, "\n"); i >= 0 {
			last = last[i+1:]
		}
		d.notify(fmt.Sprintf("swarm %s: job %s exit %d: %s", d.Cfg.Profile.Name, name, exit, clip(last, 300)))
	}
}

// AgentRun works one agent job (coordinator or node): exit 0 when the model finished, 1 when
// its CLI reported the run as an error, 124 over the timeout, 125 when parent ended, 127 when
// the account could not be set up or the CLI could not run. The text is its final answer.
func AgentRun(parent context.Context, run func(context.Context, string, string) (provider.AgentResult, error),
	setupErr error, prompt, dir string, timeout time.Duration) (int, string, float64) {
	if setupErr != nil {
		return 127, setupErr.Error(), 0
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	res, err := run(ctx, prompt, dir)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil:
		return 124, "[killed: over its timeout of " + timeout.String() + "]", 0
	case ctx.Err() != nil:
		return 125, "[killed: stopping]", 0
	case err != nil:
		return 127, err.Error(), 0
	case res.Failed:
		return 1, res.Text, res.CostUSD
	}
	return 0, res.Text, res.CostUSD
}

// RunJob starts a job by hand (`swarm jobs run`), whatever its schedule says.
func (d *Daemon) RunJob(ctx context.Context, name string) (any, error) {
	id, err := d.startJob(ctx, name, 0)
	if err != nil {
		return nil, err
	}
	return map[string]any{"job": name, "run": id, "status": "started"}, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
