package rendezvous

import (
	"net"

	"golang.org/x/sys/windows"
)

const (
	ipDontFragment = 14 // IP_DONTFRAGMENT
	ipv6DontFrag   = 14 // IPV6_DONTFRAG
)

// setDontFragment makes the relay send with the DF bit (see df_linux.go).
func setDontFragment(c *net.UDPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var e4, e6 error
	if err := rc.Control(func(fd uintptr) {
		e4 = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipDontFragment, 1)
		e6 = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, ipv6DontFrag, 1)
	}); err != nil {
		return err
	}
	if e4 != nil && e6 != nil {
		return e4
	}
	return nil
}
