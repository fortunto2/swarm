package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
)

func taskCmd() *cobra.Command {
	var repo, title, spec, acceptance, tier, topic string
	var files []string
	var needs []int64
	var priority int
	var run int64
	add := &cobra.Command{
		Use:   "add",
		Short: "Add a ready task (a new run unless --run)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			r, ok := c.Repos[repo]
			if !ok {
				return fmt.Errorf("profile has no repo %q", repo)
			}
			if spec == "-" {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				spec = string(b)
			}
			in := board.AddTaskIn{Repo: repo, Title: title, Spec: spec, Acceptance: acceptance, Tier: tier,
				Files: files, Needs: needs, Run: run, Priority: priority, Topic: topic}
			ctx := context.Background()
			// One writer: while the daemon runs, the task goes in through it.
			run, id, viaDaemon, err := addViaDaemon(c, in)
			if err != nil {
				return err
			}
			if !viaDaemon {
				d, err := db.Open(c.DBPath())
				if err != nil {
					return err
				}
				defer d.Close()
				if run == 0 {
					base, err := gitx.Head(ctx, r.Path, "refs/heads/"+r.MainBranch)
					if err != nil {
						return err
					}
					if run, err = d.CreateRun(ctx, "task:"+title, topic, base); err != nil {
						return err
					}
				}
				if id, err = d.AddTask(ctx, db.NewTask{RunID: run, Repo: repo, Title: title, Spec: spec,
					Acceptance: acceptance, Tier: tier, Priority: priority, Files: files, Needs: needs}); err != nil {
					return err
				}
			}
			state := "ready"
			if len(needs) > 0 {
				state = "draft, waiting on its dependencies"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %d task %d %s\n", run, id, state)
			return nil
		},
	}
	f := add.Flags()
	f.StringVar(&repo, "repo", "", "repo name from the profile")
	f.StringVar(&title, "title", "", "one line")
	f.StringVar(&spec, "spec", "", "what to do (- reads stdin)")
	f.StringVar(&acceptance, "acceptance", "", "how it will be checked")
	f.StringVar(&tier, "tier", "B", "S | A | B")
	f.StringVar(&topic, "topic", "", "harness topic id this run serves")
	f.StringSliceVar(&files, "files", nil, "write claims: paths or dir/ prefixes")
	f.IntVar(&priority, "priority", 0, "higher first")
	f.Int64SliceVar(&needs, "needs", nil, "task ids that must merge first; the task waits as draft")
	f.Int64Var(&run, "run", 0, "add to an existing run")
	for _, n := range []string{"repo", "title", "spec", "acceptance"} {
		_ = add.MarkFlagRequired(n)
	}
	t := &cobra.Command{Use: "task", Short: "Tasks"}
	t.AddCommand(add)
	return t
}

func daemonCmd() *cobra.Command {
	var untilIdle bool
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run swarmd for this profile in the foreground",
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
			self, err := os.Executable()
			if err != nil {
				return err
			}
			release, err := dispatch.Lock(c.StatePath())
			if err != nil {
				return err
			}
			defer release()
			dm, err := dispatch.New(c, d, self)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return dm.Run(ctx, untilIdle)
		},
	}
	c.Flags().BoolVar(&untilIdle, "until-idle", false, "exit when no run has work left and no worker is alive")
	return c
}

func mcpCmd() *cobra.Command {
	var sock, token string
	c := &cobra.Command{
		Use: "mcp",
		Short: "Stdio MCP server. With --token: a worker's board. Without: the session tools " +
			"(register once: claude mcp add swarm -- swarm mcp --profile <dir>)",
		RunE: func(*cobra.Command, []string) error {
			if token == "" {
				token = os.Getenv("SWARM_TOKEN") // how a worker's MCP config passes it
			}
			if token != "" {
				cl, err := board.Dial(sock)
				if err != nil {
					return err
				}
				defer cl.Close()
				return board.NewMCP(cl, token, "worker").Run(context.Background(), &mcp.StdioTransport{})
			}
			c, err := load()
			if err != nil {
				return err
			}
			op, err := board.OperatorToken(c.StatePath())
			if err != nil {
				return err
			}
			cl, err := board.Dial(dispatch.SocketPath(c.StatePath()))
			if err != nil {
				return fmt.Errorf("swarmd is not running for profile %s: start `swarm daemon` (%w)", c.Profile.Name, err)
			}
			defer cl.Close()
			return board.NewMCP(cl, op, "lead").Run(context.Background(), &mcp.StdioTransport{})
		},
	}
	c.Flags().StringVar(&sock, "socket", "", "swarmd socket")
	c.Flags().StringVar(&token, "token", "", "this attempt's token")
	return c
}

// report: the board without MCP, for scripted workers and debugging. Socket and token come
// from the worker's environment.
func boardCmd() *cobra.Command {
	c := &cobra.Command{Use: "board", Short: "Talk to the board from a worker's shell (SWARM_SOCKET, SWARM_TOKEN)"}
	call := func(op string, in any) error {
		cl, err := board.Dial(os.Getenv("SWARM_SOCKET"))
		if err != nil {
			return err
		}
		defer cl.Close()
		res, err := cl.Do(op, os.Getenv("SWARM_TOKEN"), in)
		if err != nil {
			var be *board.Error
			if errors.As(err, &be) {
				b, _ := json.Marshal(be)
				return errors.New(string(b))
			}
			return err
		}
		fmt.Println(string(res))
		return nil
	}
	c.AddCommand(&cobra.Command{Use: "done <commit> [summary]", Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, a []string) error {
			in := map[string]string{"commit_sha": a[0]}
			if len(a) > 1 {
				in["summary"] = a[1]
			}
			return call("report_done", in)
		}})
	c.AddCommand(&cobra.Command{Use: "blocked <reason> <question>", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			return call("report_blocked", map[string]string{"reason": a[0], "question": a[1]})
		}})
	c.AddCommand(&cobra.Command{Use: "task", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return call("get_task", nil) }})
	return c
}

// gateCmd: inside a worker's worktree, the checks its submission will face, run the same
// way the daemon runs them. Exit 1 on a failure, with the reason and the output tail.
func gateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gate",
		Short: "In a worker's worktree: run the checks the submission will face",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := board.Dial(os.Getenv("SWARM_SOCKET"))
			if err != nil {
				return fmt.Errorf("no board (run this inside a swarm worker): %w", err)
			}
			defer cl.Close()
			raw, err := cl.Do("gate_spec", os.Getenv("SWARM_TOKEN"), nil)
			if err != nil {
				return err
			}
			var spec board.GateSpec
			if err := json.Unmarshal(raw, &spec); err != nil {
				return err
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			ctx := context.Background()
			out := cmd.OutOrStdout()
			if head, err := gitx.Head(ctx, dir, "HEAD"); err == nil && head != spec.Base {
				if v := gate.Static(ctx, dir, spec.Base, head, spec.Files, gate.Grants{Tests: spec.AllowTests, Protected: spec.Protected}); !v.OK {
					fmt.Fprintf(out, "gate: FAIL (%s) %s\n%s\n", v.Stage, v.Reason, v.Tail)
					return errors.New("static checks failed")
				}
			} else {
				fmt.Fprintln(out, "gate: nothing committed yet; static checks run on your commits")
			}
			v := gate.Run(ctx, dir, spec.Commands, time.Duration(spec.TimeoutSec)*time.Second, spec.Reruns)
			if !v.OK {
				fmt.Fprintf(out, "gate: FAIL %s\n%s\n", v.Reason, v.Tail)
				return errors.New("gate failed")
			}
			fmt.Fprintln(out, "gate: pass")
			return nil
		},
	}
}

// addViaDaemon sends the task to a running daemon under the operator token. No daemon
// listening: viaDaemon is false and the caller writes the database itself (it is then the
// only writer). A daemon that answers with an error is an error, never a fallback.
func addViaDaemon(c *config.Config, in board.AddTaskIn) (run, id int64, viaDaemon bool, err error) {
	cl, err := board.Dial(dispatch.SocketPath(c.StatePath()))
	if err != nil {
		if dispatch.Running(c.StatePath()) {
			// never write around a live daemon: it is the only writer
			return 0, 0, true, fmt.Errorf("a daemon holds this profile but its socket does not answer: %w", err)
		}
		if in.Title == "" || in.Spec == "" || in.Acceptance == "" {
			return 0, 0, false, errors.New("title, spec and acceptance must not be empty")
		}
		if len(in.Files) == 0 {
			return 0, 0, false, errors.New(`--files is required (dir/ for a directory, "**" for the whole repo)`)
		}
		return in.Run, 0, false, nil
	}
	defer cl.Close()
	op, err := board.OperatorToken(c.StatePath())
	if err != nil {
		return 0, 0, true, err
	}
	raw, err := cl.Do("swarm_add_task", op, in)
	if err != nil {
		return 0, 0, true, err
	}
	var out struct{ Run, Task int64 }
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, true, err
	}
	return out.Run, out.Task, true, nil
}
