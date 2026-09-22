package cli

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

	"github.com/ikeji/mysmallnetwork/internal/buildinfo"
	"github.com/ikeji/mysmallnetwork/internal/ident"
	"github.com/ikeji/mysmallnetwork/internal/peer"
	"github.com/ikeji/mysmallnetwork/internal/proto"
	"github.com/ikeji/mysmallnetwork/internal/resume"
	"github.com/ikeji/mysmallnetwork/internal/tunnel"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// policy decides which targets clients may reach, per protocol.
type policy struct {
	tcp []target // the first is the default target
	udp []target // the first is the default target
	all bool     // any host:port, both protocols
}

// target is "[host:]port" or "[host:]lo-hi" (a port range).
type target struct {
	host   string
	lo, hi int
}

func (t target) String() string { return net.JoinHostPort(t.host, strconv.Itoa(t.lo)) }

func (t target) contains(host string, port int) bool {
	return t.host == host && port >= t.lo && port <= t.hi
}

func parseTarget(s string) (target, error) {
	host, ports := "localhost", s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, ports = s[:i], s[i+1:]
		host = strings.Trim(host, "[]") // IPv6 literal
	}
	lo, hi, ok := strings.Cut(ports, "-")
	if !ok {
		hi = lo
	}
	l, err1 := strconv.Atoi(lo)
	h, err2 := strconv.Atoi(hi)
	if host == "" || err1 != nil || err2 != nil || l < 1 || h > 65535 || l > h {
		return target{}, fmt.Errorf("bad target %q (want [host:]port or [host:]lo-hi)", s)
	}
	return target{host: host, lo: l, hi: h}, nil
}

// resolve maps a client's request to a "host:port" for proto ("tcp" or
// "udp"). Requests are "" (default), "port", "host:port", or "~port": a port
// the client did not choose itself (SOCKS5 always sends one, e.g. 80 for
// http://NAME/). A "~port" that is not exported still reaches an exporter
// that publishes exactly one target, because such a name denotes a service,
// not a host.
func (p *policy) resolve(proto, req string) (string, error) {
	targets := p.tcp
	if proto == "udp" {
		targets = p.udp
	}
	if hint, ok := strings.CutPrefix(req, "~"); ok {
		if t, err := p.resolve(proto, hint); err == nil {
			return t, nil
		} else if len(targets) == 1 && !p.all {
			return targets[0].String(), nil
		} else {
			return "", err
		}
	}
	if req == "" {
		if len(targets) == 0 {
			return "", fmt.Errorf("exporter has no default %s target", proto)
		}
		return targets[0].String(), nil
	}
	host, ports := "", req
	if i := strings.LastIndex(req, ":"); i >= 0 {
		host, ports = strings.Trim(req[:i], "[]"), req[i+1:]
	}
	port, err := strconv.Atoi(ports)
	if err != nil {
		return "", fmt.Errorf("bad target %q", req)
	}
	if host == "" { // port only: any exported target with that port, else localhost
		for _, t := range targets {
			if t.contains(t.host, port) {
				return net.JoinHostPort(t.host, ports), nil
			}
		}
		host = "localhost"
	}
	for _, t := range targets {
		if t.contains(host, port) {
			return net.JoinHostPort(host, ports), nil
		}
	}
	if p.all {
		return net.JoinHostPort(host, ports), nil
	}
	return "", fmt.Errorf("%s target %s is not exported", proto, net.JoinHostPort(host, ports))
}

// Export publishes local services under a name.
func Export(args []string) {
	fs := flag.NewFlagSet("msnw export", flag.ExitOnError)
	quietQUIC()
	name := fs.String("n", "", "name to export under (required)")
	var tcpTargets, udpTargets multiFlag
	fs.Var(&tcpTargets, "t", "TCP target to export: [host:]port or [host:]lo-hi (repeatable; first is the default)")
	fs.Var(&udpTargets, "u", "UDP target to export: [host:]port or [host:]lo-hi (repeatable; first is the default)")
	all := fs.Bool("all", false, "let clients connect to any host:port (TCP and UDP) through this exporter")
	server := fs.String("s", envOr("MSNW_SERVER", DefaultServer), "rendezvous server host:port (or $MSNW_SERVER)")
	linkKey := fs.String("key", os.Getenv("MSNW_KEY"), "link key shared with clients (or $MSNW_KEY); required")
	serverKey := fs.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key (or $MSNW_SERVER_KEY), if the server requires one")
	serverFP := fs.String("server-fp", os.Getenv("MSNW_SERVER_FP"), "pin the server's sha256 fingerprint (or $MSNW_SERVER_FP)")
	port := fs.Int("port", 0, "local UDP port to bind (0 = random)")
	genKey := fs.Bool("gen-key", false, "print a fresh random link key and exit")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Parse(args)

	if *genKey {
		fmt.Println(ident.GenerateKey())
		return
	}
	if *name == "" || *linkKey == "" || (len(tcpTargets) == 0 && len(udpTargets) == 0 && !*all) {
		fmt.Fprintln(os.Stderr, "usage: msnw export -n NAME -t [host:]port [-t ...] [-u [host:]port ...] [--all] -key LINKKEY [-s server:port] [-server-key K]")
		os.Exit(2)
	}
	pol := &policy{all: *all}
	for _, t := range tcpTargets {
		ht, err := parseTarget(t)
		if err != nil {
			log.Fatal(err)
		}
		pol.tcp = append(pol.tcp, ht)
	}
	for _, t := range udpTargets {
		ht, err := parseTarget(t)
		if err != nil {
			log.Fatal(err)
		}
		pol.udp = append(pol.udp, ht)
	}
	defaultPort := 0
	if len(pol.tcp) > 0 {
		defaultPort = pol.tcp[0].lo
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
			clientVersion, err := peer.AuthenticateAsExporter(ctx, conn, linkKey, defaultPort)
			if err != nil {
				log.Printf("peer %s rejected: %v", conn.RemoteAddr(), err)
				return
			}
			log.Printf("peer connected from %s%s", conn.RemoteAddr(), buildinfo.Mismatch("client", clientVersion, "exporter"))
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
	proto := "tcp"
	if verb == tunnel.VerbUDP {
		proto = "udp"
	}
	target, err := pol.resolve(proto, req)
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
	for flow.Idle() < exportUDPIdle {
		time.Sleep(10 * time.Second)
		if flowClosed(flow) {
			break
		}
	}
	flow.Close()
	uc.Close()
}

const exportUDPIdle = 10 * time.Minute

func flowClosed(f *tunnel.UDPFlow) bool {
	select {
	case <-f.Done():
		return true
	default:
		return false
	}
}
