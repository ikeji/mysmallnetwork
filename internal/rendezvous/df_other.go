//go:build !linux && !darwin && !freebsd && !windows

package rendezvous

import (
	"errors"
	"net"
)

func setDontFragment(c *net.UDPConn) error { return errors.New("not supported on this OS") }
