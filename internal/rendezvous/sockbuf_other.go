//go:build !unix

package rendezvous

import "net"

func socketBuffers(c *net.UDPConn) (rcv, snd int, ok bool) { return 0, 0, false }
