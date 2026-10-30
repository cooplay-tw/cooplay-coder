package agentssh

import (
	gossh "golang.org/x/crypto/ssh"
)

// PortForwardCount returns the number of open local forward channels
// (direct-tcpip and direct-streamlocal), not counting JetBrains Gateway,
// which is counted as a JetBrains session instead.
func (s *Server) PortForwardCount() int64 {
	return s.portForwards.Load()
}

// countPortForward counts the channel from its acceptance until it closes.
func (s *Server) countPortForward(newChan gossh.NewChannel) gossh.NewChannel {
	return &portForwardChannel{NewChannel: newChan, server: s}
}

type portForwardChannel struct {
	gossh.NewChannel
	server *Server
}

func (c *portForwardChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	ch, reqs, err := c.NewChannel.Accept()
	if err != nil {
		return ch, reqs, err
	}
	c.server.portForwards.Inc()
	return &ChannelOnClose{
		Channel: ch,
		done:    func() { c.server.portForwards.Dec() },
	}, reqs, nil
}
