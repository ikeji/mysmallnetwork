//go:build !linux

package rendezvous

import (
	"net"
	"sync"
)

type batchReader struct{}

func newBatchReader(c *net.UDPConn) *batchReader { return nil }

func (b *batchReader) read(pool *sync.Pool) ([]relayPacket, error) { return nil, nil }
