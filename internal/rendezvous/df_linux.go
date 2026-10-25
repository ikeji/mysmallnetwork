package rendezvous

import (
	"net"

	"golang.org/x/sys/unix"
)

// setDontFragment makes the relay send with the DF bit, like quic-go does
// (IP_PMTUDISC_PROBE: DF set, and the kernel's cached path MTU is not
// consulted, so an ICMP-learned limit does not make us refuse packets the
// path actually carries). Without it the kernel fragments datagrams larger
// than a cached MTU, and the fragments get reassembled by the peer's NAT,
// which some routers do out of order.
func setDontFragment(c *net.UDPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var e4, e6 error
	if err := rc.Control(func(fd uintptr) {
		e4 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE)
		e6 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE)
	}); err != nil {
		return err
	}
	// A v4-only socket rejects the v6 option and vice versa.
	if e4 != nil && e6 != nil {
		return e4
	}
	return nil
}
