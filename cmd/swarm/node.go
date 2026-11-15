package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/node"
)

// nodeCmd: run this machine as a node of a coordinator (its profile has [node]).
func nodeCmd() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "node",
		Short: "Run this machine as a node: dial the coordinator, run its workers here",
		Long: `Dials the coordinator with the command in [node].coordinator (ssh, usually), and
runs there the workers of the accounts the coordinator's [nodes.<name>] lists. Reconnects
when the link drops; workers keep running meanwhile. One per profile.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			if c.Node == nil {
				return errors.New("this profile has no [node]: `swarm daemon` runs a coordinator, `swarm node` a node")
			}
			if !check { // a check runs beside a live node: it takes no work and holds no lock
				release, err := dispatch.Lock(c.StatePath())
				if err != nil {
					return err
				}
				defer release()
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			n := &node.Node{Cfg: c, Bin: self, Version: revision()}
			if check {
				out, err := n.Check(ctx)
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return err
			}
			return n.Run(ctx)
		},
	}
	c.Flags().BoolVar(&check, "check", false, "dial the coordinator and ask its status, taking no work: proves ssh, the relay and the token")
	return c
}

// relayCmd: on the coordinator's host, join stdin/stdout to the daemon's socket. A node
// reaches the coordinator through it (`ssh box swarm --profile P relay --node`); the first
// line it prints is the operator token, which ssh access already grants anyway.
func relayCmd() *cobra.Command {
	var forNode bool
	c := &cobra.Command{
		Use:   "relay",
		Short: "Bridge stdin/stdout to this profile's daemon socket (the node link's transport)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := load()
			if err != nil {
				return err
			}
			sock := dispatch.SocketPath(c.StatePath())
			if forNode {
				tok, err := board.OperatorToken(c.StatePath())
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(os.Stdout, tok); err != nil {
					return err
				}
				sock = dispatch.NodeSocketPath(c.StatePath())
			}
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return fmt.Errorf("the daemon for profile %s is not running here (%s): %w", c.Profile.Name, sock, err)
			}
			defer conn.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(conn, os.Stdin); done <- struct{}{} }()
			go func() { _, _ = io.Copy(os.Stdout, conn); done <- struct{}{} }()
			<-done
			return nil
		},
	}
	c.Flags().BoolVar(&forNode, "node", false, "the node link: print the operator token, then bridge to the node socket")
	return c
}

// printNodes: the remote nodes of a coordinator, as last heard from.
func printNodes(out io.Writer, c *config.Config, d *db.DB) {
	seen := map[string]db.NodeRow{}
	if rows, err := d.Nodes(context.Background()); err == nil {
		for _, r := range rows {
			seen[r.Name] = r
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Nodes)) {
		if c.Nodes[name].Host != "remote" {
			continue
		}
		r, ok := seen[name]
		if !ok {
			fmt.Fprintf(out, "  node %-10s never connected (run `swarm node` there)\n", name)
			continue
		}
		ago := time.Since(time.Unix(r.SeenAt, 0)).Round(time.Second)
		var info struct {
			Admit   bool   `json:"admit"`
			Why     string `json:"why"`
			Running int    `json:"running"`
			Version string `json:"version"`
		}
		_ = json.Unmarshal(r.Info, &info)
		state := "online"
		if ago > c.Limits.NodeTimeout.Duration || !dispatch.Running(c.StatePath()) {
			state = "offline, last seen " + ago.String() + " ago"
		}
		line := fmt.Sprintf("  node %-10s %s, %d running", name, state, info.Running)
		if state == "online" && !info.Admit {
			line += ", holding: " + strings.TrimSpace(info.Why)
		}
		if self := revision(); info.Version != "" && self != "" && info.Version != self {
			line += fmt.Sprintf("; runs swarm %s, this binary is %s: update it there", info.Version, self)
		}
		fmt.Fprintln(out, line)
	}
}

// nodeStatus: what a node profile can say about itself; the tasks are the coordinator's.
func nodeStatus(out io.Writer, c *config.Config) error {
	state := "not running (start it: swarm node)"
	if dispatch.Running(c.StatePath()) {
		state = "running"
	}
	fmt.Fprintf(out, "node %s (profile %s): %s\n", c.Node.Name, c.Profile.Name, state)
	fmt.Fprintf(out, "  coordinator: %s\n", strings.Join(c.Node.Coordinator, " "))
	fmt.Fprintf(out, "  repos: %s\n", strings.Join(slices.Sorted(maps.Keys(c.Repos)), ", "))
	fmt.Fprintf(out, "  tasks live on the coordinator: %s status\n", strings.Join(c.Node.Coordinator, " "))
	return nil
}
