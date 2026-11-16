package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/jobs"
	"github.com/fortunto2/swarm/internal/shellx"
)

// jobState: which scheduled jobs are running now. Started from the loop and from the board
// (`swarm jobs run`), ended by their goroutine, so it has its own lock.
type jobState struct {
	mu      sync.Mutex
	running map[string]bool
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
	if len(d.Cfg.Jobs) == 0 || d.draining.Load() {
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
		if j.Spare && !d.admitting() {
			_ = d.DB.SetJobDue(ctx, name, prev.Unix())
			_, _ = d.DB.StartJobRun(ctx, name, prev.Unix(), "held: "+d.admit.why)
			d.Log.Info("job skipped for the host", "job", name, "why", d.admit.why)
			continue
		}
		if _, err := d.startJob(ctx, name, prev.Unix()); err != nil {
			continue // running already: this minute is taken when it ends and the next tick sees it
		}
		_ = d.DB.SetJobDue(ctx, name, prev.Unix())
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
		jctx, cancel := context.WithTimeout(d.prepCtx, jobTimeout(j)) // the daemon stopping stops it too
		defer cancel()
		cmd := shellx.Command(jctx, j.Run)
		cmd.Dir = dir
		// the operator's command, as hooks are: the daemon's environment, plus where it runs from
		cmd.Env = append(os.Environ(), "SWARM_JOB="+name, "SWARM_PROFILE="+d.Cfg.Dir)
		var out tail
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		exit := 0
		var ee *exec.ExitError
		switch {
		case errors.Is(jctx.Err(), context.DeadlineExceeded):
			exit = 124
			out.Write([]byte("\n[killed: over its timeout of " + jobTimeout(j).String() + "]"))
		case jctx.Err() != nil:
			exit = 125
			out.Write([]byte("\n[killed: the daemon stopped]"))
		case errors.As(err, &ee):
			exit = ee.ExitCode()
		case err != nil:
			exit = 127
			out.Write([]byte("\n" + err.Error()))
		}
		text := d.redact.Apply(out.String())
		bg := context.WithoutCancel(ctx)
		_ = d.DB.EndJobRun(bg, id, exit, text)
		d.DB.Event(bg, "swarmd", 0, "job_done", map[string]any{"job": name, "run": id, "exit": exit})
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
	}()
	return id, nil
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

// tail keeps the last 4 KB written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-4096:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
