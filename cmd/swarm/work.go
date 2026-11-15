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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/gitx"
)

func taskCmd() *cobra.Command {
	var repo, title, spec, acceptance, tier, topic string
	var files []string
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
				b, err := readAll(os.Stdin)
				if err != nil {
					return err
				}
				spec = b
			}
			d, err := db.Open(c.DBPath())
			if err != nil {
				return err
			}
			defer d.Close()
			ctx := context.Background()
			if run == 0 {
				base, err := gitx.Head(ctx, config.Expand(r.Path), "refs/heads/"+r.MainBranch)
				if err != nil {
					return err
				}
				if run, err = d.CreateRun(ctx, "task:"+title, topic, base); err != nil {
					return err
				}
			}
			id, err := d.AddTask(ctx, db.NewTask{RunID: run, Repo: repo, Title: title, Spec: spec,
				Acceptance: acceptance, Tier: tier, Priority: priority, Files: files})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run %d task %d ready\n", run, id)
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
			if token != "" {
				cl, err := board.Dial(sock)
				if err != nil {
					return err
				}
				defer cl.Close()
				return board.NewMCP(cl, token).Run(context.Background(), &mcp.StdioTransport{})
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
			return board.NewSessionMCP(cl, op).Run(context.Background(), &mcp.StdioTransport{})
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
	call := func(method string, a board.Args) error {
		a.Token = os.Getenv("SWARM_TOKEN")
		cl, err := board.Dial(os.Getenv("SWARM_SOCKET"))
		if err != nil {
			return err
		}
		defer cl.Close()
		res, err := cl.Call(method, a)
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
			s := ""
			if len(a) > 1 {
				s = a[1]
			}
			return call("ReportDone", board.Args{Commit: a[0], Summary: s})
		}})
	c.AddCommand(&cobra.Command{Use: "blocked <reason> <question>", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			return call("ReportBlocked", board.Args{Reason: a[0], Question: a[1]})
		}})
	c.AddCommand(&cobra.Command{Use: "task", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return call("GetTask", board.Args{}) }})
	return c
}

func readAll(f *os.File) (string, error) {
	b, err := io.ReadAll(f)
	return string(b), err
}
