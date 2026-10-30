package tailnet

import (
	"net/netip"
	"sync"

	"tailscale.com/wgengine"
)

// forwardCountingEngine is the engine handed to netstack. Netstack registers
// the local address of every TCP connection and UDP flow it forwards from the
// tailnet to a local port for as long as the flow lasts (UDP flows until their
// idle timeout), so the registrations count the open forwarded flows.
// Connections to a port the Conn listens on are not forwarded and not
// counted.
type forwardCountingEngine struct {
	wgengine.Engine

	mu    sync.Mutex
	flows map[netip.AddrPort]struct{}
}

func newForwardCountingEngine(engine wgengine.Engine) *forwardCountingEngine {
	return &forwardCountingEngine{
		Engine: engine,
		flows:  make(map[netip.AddrPort]struct{}),
	}
}

func (e *forwardCountingEngine) RegisterIPPortIdentity(ipport netip.AddrPort, tsIP netip.Addr) {
	e.mu.Lock()
	e.flows[ipport] = struct{}{}
	e.mu.Unlock()
	e.Engine.RegisterIPPortIdentity(ipport, tsIP)
}

func (e *forwardCountingEngine) UnregisterIPPortIdentity(ipport netip.AddrPort) {
	e.mu.Lock()
	delete(e.flows, ipport)
	e.mu.Unlock()
	e.Engine.UnregisterIPPortIdentity(ipport)
}

func (e *forwardCountingEngine) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.flows)
}

// ForwardedConnCount returns the number of TCP connections and UDP flows that
// this Conn currently forwards from the tailnet to a local port, for example
// `coder port-forward` connections and workspace app requests proxied by
// coderd. Connections to ports with a listener from Listen (SSH, reconnecting
// PTY, the agent API) are not included.
func (c *Conn) ForwardedConnCount() int {
	return c.forwards.count()
}
