package rendezvous

import (
	"net"
	"sync"

	"golang.org/x/net/ipv4"
)

// batchSize is how many packets one recvmmsg may return.
const batchSize = 64

// batchReader reads several packets per syscall (recvmmsg). The ipv4 wrapper
// only names the syscall; it works on the dual-stack socket too.
type batchReader struct {
	pc   *ipv4.PacketConn
	msgs []ipv4.Message
}

func newBatchReader(c *net.UDPConn) *batchReader {
	return &batchReader{pc: ipv4.NewPacketConn(c), msgs: make([]ipv4.Message, batchSize)}
}

// read returns the packets of one batch, each in a buffer from pool.
func (b *batchReader) read(pool *sync.Pool) ([]relayPacket, error) {
	for i := range b.msgs {
		switch m := &b.msgs[i]; {
		case m.Buffers == nil:
			m.Buffers = [][]byte{pool.Get().([]byte)}
		case m.Buffers[0] == nil:
			m.Buffers[0] = pool.Get().([]byte)
		}
	}
	n, err := b.pc.ReadBatch(b.msgs, 0)
	if err != nil {
		return nil, err
	}
	pkts := make([]relayPacket, 0, n)
	for i := 0; i < n; i++ {
		m := &b.msgs[i]
		from, ok := m.Addr.(*net.UDPAddr)
		if !ok {
			continue // keep the buffer for the next batch
		}
		pkts = append(pkts, relayPacket{b: m.Buffers[0], n: m.N, from: from})
		m.Buffers[0] = nil // handed over; a fresh buffer is fetched next time
	}
	return pkts, nil
}
