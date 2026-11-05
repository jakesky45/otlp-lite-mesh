package udp

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

// setMulticastTTL sets how many routers a multicast datagram may cross.
//
// LEARN: the standard net package deliberately exposes only portable socket
// behaviour. Multicast TTL is a socket option, so it lives in the
// golang.org/x/net extension, which wraps the UDPConn to reach it.
func setMulticastTTL(conn *net.UDPConn, ttl int) error {
	if err := ipv4.NewPacketConn(conn).SetMulticastTTL(ttl); err != nil {
		return fmt.Errorf("udp: set multicast TTL %d: %w", ttl, err)
	}
	return nil
}
