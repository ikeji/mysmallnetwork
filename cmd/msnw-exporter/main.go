// msnw-exporter: publishes local TCP services under a name.
package main

import (
	"bufio"
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
	"mysmallnetwork/internal/resume"
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

// DefaultServer is the public rendezvous server used when -s / $MSNW_SERVER
// is not given, so a downloaded binary works without running a server.
const DefaultServer = "relay.ikeji.ma:4433"

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
	server := flag.String("s", envOr("MSNW_SERVER", DefaultServer), "rendezvous server host:port (or $MSNW_SERVER)")
	linkKey := flag.String("key", os.Getenv("MSNW_KEY"), "link key shared with clients (or $MSNW_KEY); required")
	serverKey := flag.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key (or $MSNW_SERVER_KEY), if the server requires one")
	serverFP := flag.String("server-fp", os.Getenv("MSNW_SERVER_FP"), "pin the server's sha256 fingerprint (or $MSNW_SERVER_FP)")
	port := flag.Int("port", 0, "local UDP port to bind (0 = random)")
	genKey := flag.Bool("gen-key", false, "print a fresh random link key and exit")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	if *genKey {
		fmt.Println(ident.GenerateKey())
		return
	}
	if *name == "" || *linkKey == "" || (len(targets) == 0 && !*all) {
		fmt.Fprintln(os.Stderr, "usage: msnw-exporter -n NAME -t [host:]port [-t ...] [--all] -key LINKKEY [-s server:port] [-server-key K]")
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
	node, err := peer.New(id, *server, *serverKey, *serverFP, *port)
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
	sessions := resume.NewRegistry()
	go sessions.Run(ctx)
	go acceptLoop(ctx, ln, pol, *linkKey, defaultPort, sessions)

	onIncoming := func(m *proto.Message) {
		log.Printf("incoming client %s… candidates=%v", m.PeerFingerprint[:12], m.Candidates)
		allow.Add(m.PeerFingerprint)
		go node.Punch(ctx, m.Session, m.Candidates, 10*time.Second)
		go node.RelayHello(ctx, node.RelayAddr(m.RelayPort), m.Session, proto.RoleExporter)
	}

	backoff := time.Second
	for ctx.Err() == nil {
		err := node.Register(ctx, *linkKey, *name, onIncoming)
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

func acceptLoop(ctx context.Context, ln *quic.Listener, pol *policy, linkKey string, defaultPort int, sessions *resume.Registry) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			if err := peer.AuthenticateAsExporter(ctx, conn, linkKey, defaultPort); err != nil {
				log.Printf("peer %s rejected: %v", conn.RemoteAddr(), err)
				return
			}
			log.Printf("peer connected from %s", conn.RemoteAddr())
			mux := tunnel.NewUDPMux(conn)
			for {
				st, err := conn.AcceptStream(ctx)
				if err != nil {
					log.Printf("peer %s closed: %v", conn.RemoteAddr(), err)
					return
				}
				go serveStream(st, pol, mux, sessions)
			}
		}()
	}
}

func serveStream(st *quic.Stream, pol *policy, mux *tunnel.UDPMux, sessions *resume.Registry) {
	verb, req, rd, err := tunnel.Accept(st)
	if err != nil {
		st.CancelRead(0)
		st.Close()
		return
	}
	if verb == tunnel.VerbResume {
		serveResume(st, rd, req, sessions)
		return
	}
	token := ""
	if verb == tunnel.VerbConnect {
		f := strings.Fields(req)
		if len(f) != 2 {
			tunnel.Reject(st, "bad CONNECT request")
			return
		}
		req, token = f[0], f[1]
		if req == "-" {
			req = ""
		}
	}
	target, err := pol.resolve(req)
	if err != nil {
		tunnel.Reject(st, err.Error())
		return
	}
	if verb == tunnel.VerbUDP {
		serveUDP(st, rd, mux, target)
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
	sess := resume.New(token)
	sessions.Add(sess)
	if err := sess.Attach(st, rd, 0); err != nil {
		c.Close()
		return
	}
	tunnel.PipeConns(sess, c)
	sessions.Remove(token)
}

// serveResume re-attaches an existing session: "RESUME <token> <received>".
func serveResume(st *quic.Stream, rd *bufio.Reader, req string, sessions *resume.Registry) {
	var token string
	var peerRecv uint64
	if _, err := fmt.Sscanf(req, "%s %d", &token, &peerRecv); err != nil {
		tunnel.Reject(st, "bad RESUME request")
		return
	}
	sess := sessions.Get(token)
	if sess == nil || !sess.CanResume(peerRecv) {
		tunnel.Reject(st, "no such session")
		return
	}
	if _, err := fmt.Fprintf(st, "OK %d\n", sess.Recv()); err != nil {
		return
	}
	if err := sess.Attach(st, rd, peerRecv); err != nil {
		log.Printf("resume %s: %v", token[:8], err)
		return
	}
	log.Printf("session %s resumed", token[:8])
}

// serveUDP relays one UDP flow to target until the client closes it or it
// stays idle for udpIdle.
func serveUDP(st *quic.Stream, rd *bufio.Reader, mux *tunnel.UDPMux, target string) {
	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		tunnel.Reject(st, err.Error())
		return
	}
	uc, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		tunnel.Reject(st, err.Error())
		return
	}
	flow, err := mux.Accept(st, rd, func(p []byte) { uc.Write(p) })
	if err != nil {
		uc.Close()
		return
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := uc.Read(buf)
			if err != nil {
				flow.Close()
				return
			}
			if err := flow.Send(buf[:n]); err != nil {
				flow.Close()
				return
			}
		}
	}()
	for flow.Idle() < udpIdle {
		time.Sleep(10 * time.Second)
		if flowClosed(flow) {
			break
		}
	}
	flow.Close()
	uc.Close()
}

const udpIdle = 10 * time.Minute

func flowClosed(f *tunnel.UDPFlow) bool {
	select {
	case <-f.Done():
		return true
	default:
		return false
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
