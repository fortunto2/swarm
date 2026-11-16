package jobs

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/fortunto2/swarm/internal/shellx"
)

// Exec runs one job command (sh -c) in dir with env, killed as a group at timeout or when
// parent ends, and returns its exit code and the last 4 KB of its output. 124: over its
// timeout; 125: parent ended (the daemon or node stopping); 127: it could not start.
func Exec(parent context.Context, run, dir string, env []string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := shellx.Command(ctx, run)
	cmd.Dir = dir
	cmd.Env = env
	var out Tail
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil:
		out.Write([]byte("\n[killed: over its timeout of " + timeout.String() + "]"))
		return 124, out.String()
	case ctx.Err() != nil:
		out.Write([]byte("\n[killed: stopping]"))
		return 125, out.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), out.String()
	case err != nil:
		out.Write([]byte("\n" + err.Error()))
		return 127, out.String()
	}
	return 0, out.String()
}

// Tail keeps the last 4 KB written to it.
type Tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-4096:]...)
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
