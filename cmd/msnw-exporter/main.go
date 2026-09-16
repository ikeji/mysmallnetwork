// msnw-exporter: publishes local TCP services under a name.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"mysmallnetwork/internal/ident"
	"mysmallnetwork/internal/peer"
	"mysmallnetwork/internal/proto"
	"mysmallnetwork/internal/tunnel"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// policy decides which targets clients may reach.
type policy struct {
	targets []string // host:port
	all     bool
}

func parseTarget(s string) (string, error) {
	if !strings.Contains(s, ":") {
		if _, err := strconv.Atoi(s); err != nil {
			return "", fmt.Errorf("bad target %q (want port or host:port)", s)
		}
		return "localhost:" + s, nil
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return "", fmt.Errorf("bad target %q: %v", s, err)
	}
	return s, nil
}

func (p *policy) resolve(req string) (string, error) {
	if req == "" {
		if len(p.targets) == 0 {
			return "", fmt.Errorf("exporter has no default target")
		}
		return p.targets[0], nil
	}
	if !strings.Contains(req, ":") { // port only
		for _, t := range p.targets {
			if _, port, _ := net.SplitHostPort(t); port == req {
				return t, nil
			}
		}
		if p.all {
			return "localhost:" + req, nil
		}
		return "", fmt.Errorf("port %s is not exported", req)
	}
	for _, t := range p.targets {
		if t == req {
			return t, nil
		}
	}
	if p.all {
		return req, nil
	}
	return "", fmt.Errorf("target %s is not exported", req)
}

func main() {
	// quic-go warns loudly when the UDP receive buffer is small; the warning is
	// harmless for a tunnel of this size, and sysctl advice lives in the README.
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	}
	name := flag.String("n", "", "name to export under (required)")
	var targets multiFlag
	flag.Var(&targets, "t", "target to export: port or host:port (repeatable; first is the default)")
	all := flag.Bool("all", false, "let clients connect to any host:port through this exporter")
	server := flag.String("s", envOr("MSNW_SERVER", "localhost:4433"), "rendezvous server host:port (or $MSNW_SERVER)")
	secret := flag.String("secret", os.Getenv("MSNW_SECRET"), "shared secret (or $MSNW_SECRET)")
	serverFP := flag.String("server-fp", os.Getenv("MSNW_SERVER_FP"), "pin the server's sha256 fingerprint (or $MSNW_SERVER_FP)")
	port := flag.Int("port", 0, "local UDP port to bind (0 = random)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	if *name == "" || *secret == "" || (len(targets) == 0 && !*all) {
		fmt.Fprintln(os.Stderr, "usage: msnw-exporter -n NAME -t [host:]port [-t ...] [--all] [-s server:port] -secret S")
		os.Exit(2)
	}
	pol := &policy{all: *all}
	for _, t := range targets {
		ht, err := parseTarget(t)
		if err != nil {
			log.Fatal(err)
		}
		pol.targets = append(pol.targets, ht)
	}
	defaultPort := 0
	if len(pol.targets) > 0 {
		_, p, _ := net.SplitHostPort(pol.targets[0])
		defaultPort, _ = strconv.Atoi(p)
	}

	id, err := ident.New()
	if err != nil {
		log.Fatal(err)
	}
	node, err := peer.New(id, *server, *secret, *serverFP, *port)
	if err != nil {
		log.Fatal(err)
	}
	node.Verbose = *verbose
	defer node.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	allow := ident.NewAllowList(5 * time.Minute)
	ln, err := node.Listen(allow.Allowed)
	if err != nil {
		log.Fatal(err)
	}
	go acceptLoop(ctx, ln, pol)

	onIncoming := func(m *proto.Message) {
		log.Printf("incoming client %s… candidates=%v", m.PeerFingerprint[:12], m.Candidates)
		allow.Add(m.PeerFingerprint)
		go node.Punch(ctx, m.Session, m.Candidates, 10*time.Second)
		go node.RelayHello(ctx, node.RelayAddr(m.RelayPort), m.Session, proto.RoleExporter)
	}

	backoff := time.Second
	for ctx.Err() == nil {
		err := node.Register(ctx, *name, defaultPort, onIncoming)
		if ctx.Err() != nil {
			break
		}
		log.Printf("disconnected from server: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func acceptLoop(ctx context.Context, ln *quic.Listener, pol *policy) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		log.Printf("peer connected from %s", conn.RemoteAddr())
		go func() {
			for {
				st, err := conn.AcceptStream(ctx)
				if err != nil {
					log.Printf("peer %s closed: %v", conn.RemoteAddr(), err)
					return
				}
				go serveStream(st, pol)
			}
		}()
	}
}

func serveStream(st *quic.Stream, pol *policy) {
	req, rd, err := tunnel.Accept(st)
	if err != nil {
		st.CancelRead(0)
		st.Close()
		return
	}
	target, err := pol.resolve(req)
	if err != nil {
		tunnel.Reject(st, err.Error())
		return
	}
	c, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		tunnel.Reject(st, err.Error())
		return
	}
	if err := tunnel.Confirm(st); err != nil {
		c.Close()
		return
	}
	tunnel.Pipe(st, rd, c)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
