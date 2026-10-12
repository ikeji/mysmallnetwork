package peer

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ikeji/mysmallnetwork/internal/ident"
)

// TestLateIntroduction reproduces an importer on the exporter's LAN whose
// handshake arrives before the server's introduction: the exporter must hold
// the handshake until the fingerprint is added rather than reject it.
func TestLateIntroduction(t *testing.T) {
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
	allow := ident.NewAllowList(time.Minute)
	ln, err := ts.Listen(idS.ServerConfig(allow.Await), peerConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accepted := make(chan error, 1)
	go func() {
		s, err := ln.Accept(ctx)
		if err == nil {
			_, err = AuthenticateAsExporter(ctx, s, "k", 22)
		}
		accepted <- err
	}()
	go func() {
		time.Sleep(500 * time.Millisecond) // the introduction crossing the internet
		allow.Add(idC.Fingerprint)
	}()
	c, err := tc.Dial(ctx, us.LocalAddr(), idC.ClientConfig(idS.Fingerprint), peerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := AuthenticateAsClient(ctx, c, "k"); err != nil {
		t.Fatalf("importer rejected despite the late introduction: %v", err)
	}
	if err := <-accepted; err != nil {
		t.Fatalf("exporter side: %v", err)
	}
}

// TestNoIntroduction checks that an importer the server never introduced is
// still turned away once the grace period is over.
func TestNoIntroduction(t *testing.T) {
	old := ident.IntroductionGrace
	ident.IntroductionGrace = 200 * time.Millisecond
	defer func() { ident.IntroductionGrace = old }()
	idC, _ := ident.New()
	idS, _ := ident.New()
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	us, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	tc, ts := &quic.Transport{Conn: uc}, &quic.Transport{Conn: us}
	t.Cleanup(func() { tc.Close(); ts.Close() })
	allow := ident.NewAllowList(time.Minute)
	ln, err := ts.Listen(idS.ServerConfig(allow.Await), peerConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		if s, err := ln.Accept(ctx); err == nil {
			t.Errorf("exporter accepted an importer that was never introduced (%s)", s.RemoteAddr())
		}
	}()
	c, err := tc.Dial(ctx, us.LocalAddr(), idC.ClientConfig(idS.Fingerprint), peerConfig())
	if err != nil {
		return // rejected during the handshake: fine
	}
	if _, _, err := AuthenticateAsClient(ctx, c, "k"); err == nil {
		t.Fatal("importer authenticated without an introduction")
	}
}
