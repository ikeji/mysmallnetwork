package tunnel

// UDP forwarding. A flow is one (local source -> target) association:
//
//	client:   "UDP <target>\n"          target = "port" | "host:port"
//	exporter: "OK <flow id>\n" | "ERR <reason>\n"
//
// Payloads then travel as QUIC datagrams "[varint flow id][payload]" when they
// fit and the peer supports them, otherwise as "[uint16 length][payload]"
// frames on the flow's own stream. Closing the stream ends the flow.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// UDPMux multiplexes UDP flows over one peer connection.
type UDPMux struct {
	conn  *quic.Conn
	mu    sync.Mutex
	flows map[uint64]*UDPFlow
	next  uint64
	dgram bool
}

// UDPFlow is one forwarded UDP association.
type UDPFlow struct {
	mux  *UDPMux
	ID   uint64
	st   *quic.Stream
	rd   *bufio.Reader
	recv func([]byte)
	wmu  sync.Mutex
	once sync.Once
	done chan struct{}
	// LastSeen is updated on every payload in either direction.
	LastSeen time.Time
	lsmu     sync.Mutex
}

// NewUDPMux wraps conn and starts dispatching incoming datagrams.
func NewUDPMux(conn *quic.Conn) *UDPMux {
	cs := conn.ConnectionState()
	m := &UDPMux{conn: conn, flows: map[uint64]*UDPFlow{}, dgram: cs.SupportsDatagrams.Local && cs.SupportsDatagrams.Remote}
	if m.dgram {
		go m.recvLoop()
	}
	return m
}

func (m *UDPMux) recvLoop() {
	for {
		b, err := m.conn.ReceiveDatagram(context.Background())
		if err != nil {
			return
		}
		id, n := binary.Uvarint(b)
		if n <= 0 {
			continue
		}
		m.mu.Lock()
		f := m.flows[id]
		m.mu.Unlock()
		if f != nil {
			f.deliver(b[n:])
		}
	}
}

func (m *UDPMux) add(f *UDPFlow) {
	m.mu.Lock()
	m.flows[f.ID] = f
	m.mu.Unlock()
	go f.streamLoop()
}

func (m *UDPMux) remove(id uint64) {
	m.mu.Lock()
	delete(m.flows, id)
	m.mu.Unlock()
}

// Open (client side) asks the exporter for a flow to target. recv is called
// for every payload coming back.
func (m *UDPMux) Open(ctx context.Context, target string, recv func([]byte)) (*UDPFlow, error) {
	st, err := m.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(st, "UDP %s\n", target); err != nil {
		st.Close()
		return nil, err
	}
	rd := bufio.NewReader(st)
	st.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := rd.ReadString('\n')
	st.SetReadDeadline(time.Time{})
	if err != nil {
		st.Close()
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	var id uint64
	if _, err := fmt.Sscanf(line, "OK %d", &id); err != nil {
		st.CancelRead(0)
		st.Close()
		return nil, errors.New(strings.TrimPrefix(line, "ERR "))
	}
	f := &UDPFlow{mux: m, ID: id, st: st, rd: rd, recv: recv, LastSeen: time.Now(), done: make(chan struct{})}
	m.add(f)
	return f, nil
}

// Accept (exporter side) completes a "UDP" request already parsed by Accept,
// allocating a flow id and answering OK.
func (m *UDPMux) Accept(st *quic.Stream, rd *bufio.Reader, recv func([]byte)) (*UDPFlow, error) {
	m.mu.Lock()
	m.next++
	id := m.next
	m.mu.Unlock()
	if _, err := fmt.Fprintf(st, "OK %d\n", id); err != nil {
		return nil, err
	}
	f := &UDPFlow{mux: m, ID: id, st: st, rd: rd, recv: recv, LastSeen: time.Now(), done: make(chan struct{})}
	m.add(f)
	return f, nil
}

// Done is closed when the flow has ended.
func (f *UDPFlow) Done() <-chan struct{} { return f.done }

func (f *UDPFlow) touch() {
	f.lsmu.Lock()
	f.LastSeen = time.Now()
	f.lsmu.Unlock()
}

// Idle reports how long the flow has carried nothing.
func (f *UDPFlow) Idle() time.Duration {
	f.lsmu.Lock()
	defer f.lsmu.Unlock()
	return time.Since(f.LastSeen)
}

func (f *UDPFlow) deliver(p []byte) {
	f.touch()
	f.recv(p)
}

// Send forwards one payload to the peer.
func (f *UDPFlow) Send(p []byte) error {
	f.touch()
	if f.mux.dgram {
		buf := make([]byte, binary.MaxVarintLen64+len(p))
		n := binary.PutUvarint(buf, f.ID)
		copy(buf[n:], p)
		err := f.mux.conn.SendDatagram(buf[:n+len(p)])
		var tooLarge *quic.DatagramTooLargeError
		if err == nil || !errors.As(err, &tooLarge) {
			return err
		}
	}
	if len(p) > 65535 {
		return errors.New("udp payload too large")
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
	if _, err := f.st.Write(hdr[:]); err != nil {
		return err
	}
	_, err := f.st.Write(p)
	return err
}

// streamLoop reads framed payloads (the fallback path) until the stream ends.
func (f *UDPFlow) streamLoop() {
	defer f.Close()
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(f.rd, hdr[:]); err != nil {
			return
		}
		p := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(f.rd, p); err != nil {
			return
		}
		f.deliver(p)
	}
}

// Close ends the flow on both sides.
func (f *UDPFlow) Close() {
	f.once.Do(func() {
		f.mux.remove(f.ID)
		f.st.CancelRead(0)
		f.st.Close()
		close(f.done)
	})
}
