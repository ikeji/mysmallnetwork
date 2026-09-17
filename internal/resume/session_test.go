package resume

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"mysmallnetwork/internal/ident"
)

// quicPair returns a connected client/server QUIC pair on loopback.
func quicPair(t *testing.T) (*quic.Conn, *quic.Conn) {
	t.Helper()
	idC, _ := ident.New()
	idS, _ := ident.New()
	cfg := &quic.Config{MaxIncomingStreams: 64, MaxIdleTimeout: 10 * time.Second}
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	us, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	tc, ts := &quic.Transport{Conn: uc}, &quic.Transport{Conn: us}
	t.Cleanup(func() { tc.Close(); ts.Close() })
	ln, err := ts.Listen(idS.ServerConfig(nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acc := make(chan *quic.Conn, 1)
	go func() {
		if c, err := ln.Accept(ctx); err == nil {
			acc <- c
		}
	}()
	c, err := tc.Dial(ctx, us.LocalAddr(), idC.ClientConfig(idS.Fingerprint), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, <-acc
}

// streamPair opens a stream from c and accepts it on s, exchanging one
// line so both sides have a buffered reader positioned at the frames.
func streamPair(t *testing.T, c, s *quic.Conn) (*quic.Stream, *bufio.Reader, *quic.Stream, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cs.Write([]byte("hello\n"))
	ss, err := s.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srd := bufio.NewReader(ss)
	if _, err := srd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ss.Write([]byte("ok\n"))
	crd := bufio.NewReader(cs)
	if _, err := crd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return cs, crd, ss, srd
}

// TestResumeAcrossStreams pushes data through a session while the stream
// carrying it is torn down and replaced several times, and checks that the
// receiver sees exactly the bytes sent, in order.
func TestResumeAcrossStreams(t *testing.T) {
	c, s := quicPair(t)
	a, b := New("tok"), New("tok")
	detached := make(chan struct{}, 16)
	a.OnDetach = func(*Session) { detached <- struct{}{} }

	cs, crd, ss, srd := streamPair(t, c, s)
	if err := b.Attach(ss, srd, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.Attach(cs, crd, 0); err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 3*1024*1024)
	rand.Read(payload)
	got := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(b)
		got <- data
	}()
	go func() {
		// Write in pieces so breaks land mid-transfer.
		for off := 0; off < len(payload); off += 100 * 1024 {
			end := min(off+100*1024, len(payload))
			if _, err := a.Write(payload[off:end]); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		a.CloseWrite()
	}()

	// Break the transport a few times while the transfer runs.
	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		cs.CancelWrite(1)
		cs.CancelRead(1)
		select {
		case <-detached:
		case <-time.After(5 * time.Second):
			t.Fatal("client side never detached")
		}
		cs, crd, ss, srd = streamPair(t, c, s)
		if !b.CanResume(a.Recv()) {
			t.Fatalf("server refuses resume at %d", a.Recv())
		}
		peerRecv := b.Recv()
		if err := b.Attach(ss, srd, a.Recv()); err != nil {
			t.Fatal(err)
		}
		if err := a.Attach(cs, crd, peerRecv); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case data := <-got:
		if !bytes.Equal(data, payload) {
			t.Fatalf("received %d bytes, want %d; equal prefix=%d", len(data), len(payload), commonPrefix(data, payload))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("transfer did not finish")
	}
}

func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// TestFinBothWays checks that a session ends cleanly when both sides finish.
func TestFinBothWays(t *testing.T) {
	c, s := quicPair(t)
	a, b := New("t2"), New("t2")
	cs, crd, ss, srd := streamPair(t, c, s)
	b.Attach(ss, srd, 0)
	a.Attach(cs, crd, 0)
	a.Write([]byte("ping"))
	a.CloseWrite()
	buf := make([]byte, 16)
	n, _ := io.ReadFull(b, buf[:4])
	if string(buf[:n]) != "ping" {
		t.Fatalf("got %q", buf[:n])
	}
	if _, err := b.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF after peer FIN, got %v", err)
	}
	b.Write([]byte("pong"))
	b.CloseWrite()
	n, _ = io.ReadFull(a, buf[:4])
	if string(buf[:n]) != "pong" {
		t.Fatalf("got %q", buf[:n])
	}
	if _, err := a.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !a.Closed() || !b.Closed() {
		t.Fatal("sessions should be closed after both FINs")
	}
}
