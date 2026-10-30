package agent

import (
	"github.com/coder/coder/v2/agent/agentsocket"
)

// ClientSessions implements agentsocket.SessionReporter. It reads the same
// counters as the stats report, live instead of per report interval, plus the
// open port-forwarded connections, which the stats report does not carry.
func (a *agent) ClientSessions() agentsocket.Sessions {
	sessions := agentsocket.Sessions{
		SSH:             a.sshServer.SessionCounts(),
		ReconnectingPTY: a.reconnectingPTYServer.ConnCount(),
		PortForward:     a.sshServer.PortForwardCount(),
	}
	if network, ok := a.requireNetwork(); ok {
		sessions.PortForward += int64(network.ForwardedConnCount())
	}
	return sessions
}
