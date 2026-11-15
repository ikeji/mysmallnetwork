package peer

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ikeji/mysmallnetwork/internal/ident"
	"github.com/ikeji/mysmallnetwork/internal/proto"
	"github.com/ikeji/mysmallnetwork/internal/rendezvous"
)

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := uc.LocalAddr().String()
	uc.Close()
	return addr
}

func startTestServer(t *testing.T) (addr, fp string) {
	t.Helper()
	id, err := ident.New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr = freeUDPAddr(t)
	srv := &rendezvous.Server{Ident: id}
	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx, addr, "127.0.0.1:0")
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return addr, id.Fingerprint
}

func startRegisteredExporter(t *testing.T, addrs, fps []string, key, name string) {
	t.Helper()
	id, err := ident.New()
	if err != nil {
		t.Fatal(err)
	}
	exp, err := New(id, addrs, "", fps, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		exp.Close()
	})
	for _, a := range addrs {
		go func(a string) {
			for ctx.Err() == nil {
				_ = exp.Register(ctx, a, key, name, func(*proto.Message, *net.UDPAddr) {})
				time.Sleep(20 * time.Millisecond)
			}
		}(a)
	}
	// Wait until a client can look the exporter up on each server individually.
	for _, a := range addrs {
		cid, _ := ident.New()
		probe, err := New(cid, []string{a}, "", fps, 0)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			lctx, lcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			_, err := probe.Lookup(lctx, key, name)
			lcancel()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				probe.Close()
				t.Fatalf("exporter never registered at %s: %v", a, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		probe.Close()
	}
}

// TestLookupBeyondStreamLimit runs well past quic-go's default 100-stream
// incoming limit on a single control connection across three servers, covering
// both hits (TypePeer) and misses (TypeError). Without stream read-side
// completion, lookup #101 hangs in OpenStreamSync until ctx's deadline.
func TestLookupBeyondStreamLimit(t *testing.T) {
	var addrs, fps []string
	for i := 0; i < 3; i++ {
		a, fp := startTestServer(t)
		addrs = append(addrs, a)
		fps = append(fps, fp)
	}
	startRegisteredExporter(t, addrs, fps, "secret", "pocket")

	id, _ := ident.New()
	client, err := New(id, addrs, "", fps, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	// Establish control connections on all three servers via a miss (which
	// waits for all three answers).
	ctx0, cancel0 := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = client.Lookup(ctx0, "secret", "missing")
	cancel0()

	initial := make([]*quic.Conn, len(client.servers))
	for i, s := range client.servers {
		s.mu.Lock()
		initial[i] = s.ctrl
		s.mu.Unlock()
		if initial[i] == nil {
			t.Fatalf("server %d has no control connection", i)
		}
	}

	const rounds = 150 // > protocol.DefaultMaxIncomingStreams (100)
	for i := 0; i < rounds; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := client.Lookup(ctx, "secret", "pocket")
		cancel()
		if err != nil {
			t.Fatalf("hit lookup #%d failed: %v", i+1, err)
		}
		if info.Name != "pocket" {
			t.Fatalf("lookup #%d name = %q, want pocket", i+1, info.Name)
		}
	}
	for i := 0; i < rounds; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := client.Lookup(ctx, "secret", "missing")
		cancel()
		if err == nil || !strings.Contains(err.Error(), "no such exporter") {
			t.Fatalf("miss lookup #%d = %v, want 'no such exporter'", i+1, err)
		}
	}

	// All 300+ lookups should have reused the original control connections
	// thanks to completed streams and MAX_STREAMS, rather than reconnecting.
	for i, s := range client.servers {
		s.mu.Lock()
		cur := s.ctrl
		s.mu.Unlock()
		if cur != initial[i] {
			t.Fatalf("server %d reconnected unexpectedly (%p -> %p)", i, initial[i], cur)
		}
	}
}

// TestLookupRecoversWhenControlStreamsExhausted simulates an older server that
// never sends MAX_STREAMS: once every server's cached control connection hits
// its outgoing stream limit, Lookup must drop the wedged connection and redial
// immediately rather than blocking until context deadline on every future call.
func TestLookupRecoversWhenControlStreamsExhausted(t *testing.T) {
	var addrs, fps []string
	for i := 0; i < 3; i++ {
		a, fp := startTestServer(t)
		addrs = append(addrs, a)
		fps = append(fps, fp)
	}
	startRegisteredExporter(t, addrs, fps, "secret", "filebrowser")

	id, _ := ident.New()
	client, err := New(id, addrs, "", fps, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	// Warm all three control connections.
	ctx0, cancel0 := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = client.Lookup(ctx0, "secret", "missing")
	cancel0()

	// Spend the entire outgoing stream quota on all three control connections
	// without closing or completing those streams.
	var held []*quic.Stream
	defer func() {
		for _, st := range held {
			st.CancelRead(0)
			st.CancelWrite(0)
		}
	}()
	for i, s := range client.servers {
		s.mu.Lock()
		ctrl := s.ctrl
		s.mu.Unlock()
		n := 0
		for {
			st, err := ctrl.OpenStream()
			if err != nil {
				break
			}
			held = append(held, st)
			n++
		}
		if n == 0 {
			t.Fatalf("server %d opened 0 streams before limit", i)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := client.Lookup(ctx, "secret", "filebrowser")
	if err != nil {
		t.Fatalf("Lookup with exhausted control streams failed: %v", err)
	}
	if info.Name != "filebrowser" {
		t.Fatalf("got %q, want filebrowser", info.Name)
	}
	_ = fmt.Sprint(info.Session)
}
