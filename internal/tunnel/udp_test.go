package tunnel

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"mysmallnetwork/internal/ident"
)

func quicPair(t *testing.T) (client, server *quic.Conn) {
	t.Helper()
	idC, _ := ident.New()
	idS, _ := ident.New()
	cfg := &quic.Config{EnableDatagrams: true, MaxIncomingStreams: 64}
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
		c, err := ln.Accept(ctx)
		if err == nil {
			acc <- c
		}
	}()
	client, err = tc.Dial(ctx, us.LocalAddr(), idC.ClientConfig(idS.Fingerprint), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return client, <-acc
}

// TestUDPFlowRoundTrip exercises the datagram path (small payload) and the
// stream fallback (payloads larger than a QUIC packet), plus echo back.
func TestUDPFlowRoundTrip(t *testing.T) {
	c, s := quicPair(t)
	ctx := context.Background()

	// exporter side: accept flows and echo payloads back with a prefix
	go func() {
		mux := NewUDPMux(s)
		for {
			st, err := s.AcceptStream(ctx)
			if err != nil {
				return
			}
			verb, target, rd, err := Accept(st)
			if err != nil || verb != VerbUDP || target != "1234" {
				t.Errorf("bad request: %v %q %q", err, verb, target)
				return
			}
			var f *UDPFlow
			f, err = mux.Accept(st, rd, func(p []byte) { f.Send(append([]byte("echo:"), p...)) })
			if err != nil {
				t.Error(err)
			}
		}
	}()

	mux := NewUDPMux(c)
	got := make(chan []byte, 8)
	f, err := mux.Open(ctx, "1234", func(p []byte) { got <- append([]byte(nil), p...) })
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, size := range []int{5, 1100, 3000, 20000} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		if err := f.Send(payload); err != nil {
			t.Fatalf("send %d: %v", size, err)
		}
		select {
		case p := <-got:
			if !bytes.Equal(p, append([]byte("echo:"), payload...)) {
				t.Fatalf("size %d: bad echo (%d bytes)", size, len(p))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("size %d: no echo", size)
		}
	}
}
