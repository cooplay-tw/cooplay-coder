package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/coder/v2/cli/cliui"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/serpent"
)

// agentSleepWakeCommand exposes the agent sleep/wake hooks over the local
// agent socket. Cooplay agent workspaces call these from the guest daemon
// around VM snapshot/restore while the workspace build stays running.
func (r *RootCmd) agentSleepWakeCommand() *serpent.Command {
	var socketPath string

	cmd := &serpent.Command{
		Use:   "agent",
		Short: "Control the local workspace agent through the agent socket",
		Long:  "Commands that talk to the Coder agent running in this workspace over its local socket. Used to prepare the agent for a host suspend, to recover its connections after the host resumes and to count its client sessions.",
		Handler: func(i *serpent.Invocation) error {
			return i.Command.HelpHandler(i)
		},
		Children: []*serpent.Command{
			r.agentPrepareSleep(&socketPath),
			r.agentWake(&socketPath),
			r.agentSessions(&socketPath),
		},
		Options: serpent.OptionSet{
			{
				Flag:        "socket-path",
				Env:         "CODER_AGENT_SOCKET_PATH",
				Description: "Specify the path for the agent socket.",
				Value:       serpent.StringOf(&socketPath),
			},
		},
	}
	return cmd
}

func (*RootCmd) agentPrepareSleep(socketPath *string) *serpent.Command {
	return &serpent.Command{
		Use:   "prepare-sleep",
		Short: "Tell the agent the host is about to be suspended",
		Long:  "Makes the agent skip the graceful coordinator disconnect when its connections are torn down, so coderd reports the agent as lost (not disconnected) and clients keep it. Cleared by 'wake'.",
		Handler: func(i *serpent.Invocation) error {
			err := withAgentSocketClient(i.Context(), *socketPath, func(ctx context.Context, client *agentsocket.Client) error {
				return client.PrepareSleep(ctx)
			})
			if err != nil {
				return xerrors.Errorf("prepare sleep: %w", err)
			}
			cliui.Info(i.Stdout, "Success")
			return nil
		},
	}
}

func (*RootCmd) agentWake(socketPath *string) *serpent.Command {
	return &serpent.Command{
		Use:   "wake",
		Short: "Tell the agent the host has resumed",
		Long:  "Drops the agent's coderd and DERP connections without a graceful disconnect, rebinds tailnet, forces WireGuard handshakes and reconnects immediately. Also clears 'prepare-sleep'.",
		Handler: func(i *serpent.Invocation) error {
			err := withAgentSocketClient(i.Context(), *socketPath, func(ctx context.Context, client *agentsocket.Client) error {
				return client.Wake(ctx)
			})
			if err != nil {
				return xerrors.Errorf("wake: %w", err)
			}
			cliui.Info(i.Stdout, "Success")
			return nil
		},
	}
}

// agentClientSessions is the output of `coder exp agent sessions --json`.
// The Cooplay guest daemon parses it strictly: exactly these six keys, with
// non-negative counts.
type agentClientSessions struct {
	SSH         int64 `json:"ssh"`
	VSCode      int64 `json:"vscode"`
	JetBrains   int64 `json:"jetbrains"`
	Other       int64 `json:"other"`
	PTY         int64 `json:"pty"`
	PortForward int64 `json:"port_forward"`
}

// summarizeAgentSessions groups SSH sessions by app family. App names outside
// the SSH, VS Code and JetBrains families are summed into Other.
func summarizeAgentSessions(s agentsocket.Sessions) agentClientSessions {
	out := agentClientSessions{
		PTY:         max(s.ReconnectingPTY, 0),
		PortForward: max(s.PortForward, 0),
	}
	for app, n := range s.SSH {
		if n <= 0 {
			continue
		}
		switch codersdk.AppNameFamily(app) {
		case codersdk.AppFamilySSH:
			out.SSH += n
		case codersdk.AppFamilyVSCode:
			out.VSCode += n
		case codersdk.AppFamilyJetBrains:
			out.JetBrains += n
		default:
			out.Other += n
		}
	}
	return out
}

func (*RootCmd) agentSessions(socketPath *string) *serpent.Command {
	var jsonOutput bool
	return &serpent.Command{
		Use:   "sessions",
		Short: "Count the client sessions the agent serves",
		Long:  "Prints the open SSH sessions by client (ssh, vscode, jetbrains, other), the connected web terminals (pty) and the open port-forwarded connections (port_forward). With --json the output is one JSON object with exactly these six keys.",
		Options: serpent.OptionSet{
			{
				Flag:        "json",
				Description: "Print one JSON object instead of a table.",
				Value:       serpent.BoolOf(&jsonOutput),
			},
		},
		Handler: func(i *serpent.Invocation) error {
			var sessions agentsocket.Sessions
			err := withAgentSocketClient(i.Context(), *socketPath, func(ctx context.Context, client *agentsocket.Client) error {
				var err error
				sessions, err = client.Sessions(ctx)
				return err
			})
			if err != nil {
				return xerrors.Errorf("get sessions: %w", err)
			}
			summary := summarizeAgentSessions(sessions)
			if jsonOutput {
				// Encode writes compact JSON and a newline.
				return json.NewEncoder(i.Stdout).Encode(summary)
			}
			tw := tabwriter.NewWriter(i.Stdout, 0, 0, 2, ' ', 0)
			for _, row := range []struct {
				name  string
				count int64
			}{
				{"SSH", summary.SSH},
				{"VS Code", summary.VSCode},
				{"JetBrains", summary.JetBrains},
				{"Other", summary.Other},
				{"Web terminal", summary.PTY},
				{"Port forward", summary.PortForward},
			} {
				_, _ = fmt.Fprintf(tw, "%s\t%d\n", row.name, row.count)
			}
			return tw.Flush()
		},
	}
}

func withAgentSocketClient(ctx context.Context, socketPath string, fn func(context.Context, *agentsocket.Client) error) error {
	opts := []agentsocket.Option{}
	if socketPath != "" {
		opts = append(opts, agentsocket.WithPath(socketPath))
	}
	client, err := agentsocket.NewClient(ctx, opts...)
	if err != nil {
		return xerrors.Errorf("connect to agent socket: %w", err)
	}
	defer client.Close()
	return fn(ctx, client)
}
