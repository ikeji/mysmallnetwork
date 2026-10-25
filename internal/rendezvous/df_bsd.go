//go:build darwin || freebsd

package rendezvous

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// setDontFragment makes the relay send with the DF bit (see df_linux.go).
// IP_DONTFRAG only takes on an AF_INET socket: on a dual-stack socket the
// kernel rejects IPPROTO_IP options and IPV6_DONTFRAG does not cover the
// IPv4-mapped sends, so those go out without DF (verified on FreeBSD 14).
func setDontFragment(c *net.UDPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var e4, e6 error
	if err := rc.Control(func(fd uintptr) {
		e4 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_DONTFRAG, 1)
		e6 = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_DONTFRAG, 1)
	}); err != nil {
		return err
	}
	if e4 != nil && e6 != nil {
		return e4
	}
	if e4 != nil && c.LocalAddr().(*net.UDPAddr).IP.To4() == nil {
		return errors.New("IPv4 sends of a dual-stack socket cannot set DF on this OS; use -listen 0.0.0.0:PORT -relay 0.0.0.0:PORT for IPv4 peers")
	}
	return nil
}
