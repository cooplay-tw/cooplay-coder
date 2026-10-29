package cli

import (
	"context"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/agent/agentsocket"
	"github.com/coder/coder/v2/cli/cliui"
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
		Long:  "Commands that talk to the Coder agent running in this workspace over its local socket. Used to prepare the agent for a host suspend and to recover its connections after the host resumes.",
		Handler: func(i *serpent.Invocation) error {
			return i.Command.HelpHandler(i)
		},
		Children: []*serpent.Command{
			r.agentPrepareSleep(&socketPath),
			r.agentWake(&socketPath),
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
