//go:build unix

package rendezvous

import (
	"net"
	"syscall"
)

// socketBuffers returns the receive and send buffer sizes the kernel granted
// for c (Linux reports twice the requested value, bookkeeping included).
func socketBuffers(c *net.UDPConn) (rcv, snd int, ok bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var e1, e2 error
	err = raw.Control(func(fd uintptr) {
		rcv, e1 = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		snd, e2 = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	})
	return rcv, snd, err == nil && e1 == nil && e2 == nil
}
