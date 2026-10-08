package cli

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ikeji/mysmallnetwork/internal/ident"
)

// loopbackConn returns the client end of a QUIC connection on loopback.
func loopbackConn(t *testing.T) *quic.Conn {
	t.Helper()
	idC, _ := ident.New()
	idS, _ := ident.New()
	cfg := &quic.Config{MaxIdleTimeout: 10 * time.Second}
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
	go ln.Accept(ctx)
	c, err := tc.Dial(ctx, us.LocalAddr(), idC.ClientConfig(idS.Fingerprint), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSweepIdle checks that an unused connection is closed once it has been
// idle long enough, and that a live lease keeps it open.
func TestSweepIdle(t *testing.T) {
	p := &pool{ents: map[string]*entry{}}
	e := p.entry("home")
	e.conn = loopbackConn(t)
	e.idleSince = time.Now().Add(-time.Hour)

	// Take a lease by hand (lease() would dial): the sweeper must leave it.
	e.users, e.idleSince = 1, time.Time{}
	p.sweepOnce(time.Minute)
	if e.conn.Context().Err() != nil {
		t.Fatal("connection closed while leased")
	}

	// Release: idle starts now, so a long threshold keeps it...
	e.users, e.idleSince = 0, time.Now()
	p.sweepOnce(time.Minute)
	if e.conn.Context().Err() != nil {
		t.Fatal("connection closed before the idle period passed")
	}
	// ...and a threshold that has passed closes it.
	e.idleSince = time.Now().Add(-2 * time.Minute)
	p.sweepOnce(time.Minute)
	if e.conn.Context().Err() == nil {
		t.Fatal("idle connection not closed")
	}
}
