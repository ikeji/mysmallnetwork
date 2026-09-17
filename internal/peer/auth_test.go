package peer

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"mysmallnetwork/internal/ident"
)

// pair dials a QUIC connection between two fresh transports on loopback.
func pair(t *testing.T) (client, server *quic.Conn) {
	t.Helper()
	idC, _ := ident.New()
	idS, _ := ident.New()
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	us, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tc, ts := &quic.Transport{Conn: uc}, &quic.Transport{Conn: us}
	t.Cleanup(func() { tc.Close(); ts.Close() })
	ln, err := ts.Listen(idS.ServerConfig(func(string) bool { return true }), peerConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan *quic.Conn, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err == nil {
			accepted <- c
		}
	}()
	client, err = tc.Dial(ctx, us.LocalAddr(), idC.ClientConfig(idS.Fingerprint), peerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return client, <-accepted
}

func runAuth(t *testing.T, clientKey, exporterKey string) (port int, cerr, serr error) {
	t.Helper()
	c, s := pair(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- AuthenticateAsExporter(ctx, s, exporterKey, 2222) }()
	port, cerr = AuthenticateAsClient(ctx, c, clientKey)
	serr = <-done
	return
}

func TestLinkKeyMatch(t *testing.T) {
	port, cerr, serr := runAuth(t, "k1", "k1")
	if cerr != nil || serr != nil {
		t.Fatalf("expected success, got client=%v exporter=%v", cerr, serr)
	}
	if port != 2222 {
		t.Fatalf("default port = %d, want 2222", port)
	}
}

func TestLinkKeyMismatch(t *testing.T) {
	_, cerr, serr := runAuth(t, "k1", "k2")
	if cerr == nil || serr == nil {
		t.Fatalf("expected both sides to fail, got client=%v exporter=%v", cerr, serr)
	}
}

func TestHashNameNamespaces(t *testing.T) {
	if ident.HashName("a", "x") == ident.HashName("b", "x") {
		t.Fatal("same name under different keys must not collide")
	}
	if ident.HashName("a", "x") != ident.HashName("a", "x") {
		t.Fatal("hash must be deterministic")
	}
}
