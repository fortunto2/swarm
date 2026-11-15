// Package shellx runs a profile's shell commands (gate commands, hooks, notify, admit) so
// that a timeout ends the whole command, not only the shell: the command gets its own
// process group, the group is killed on cancel, and a child that left the group cannot hold
// the output pipe past WaitDelay. Without it, `curl ... | grep` hanging in a hook or an admit
// command kept its goroutine (and, for admit, every new start) waiting until it exited.
package shellx

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay: how long a finished or killed command may keep its output pipe open through a
// child that escaped the group, before Wait gives up on it.
const WaitDelay = 5 * time.Second

// Command is `sh -c line args...` bound to ctx; args become $0, $1, ...
func Command(ctx context.Context, line string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", line}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = WaitDelay
	return cmd
}
