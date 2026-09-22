package cli

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
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ikeji/mysmallnetwork/internal/ident"
	"github.com/ikeji/mysmallnetwork/internal/netutil"
	"github.com/ikeji/mysmallnetwork/internal/peer"
	"github.com/ikeji/mysmallnetwork/internal/resume"
	"github.com/ikeji/mysmallnetwork/internal/socks5"
	"github.com/ikeji/mysmallnetwork/internal/tunnel"
)

// pool keeps one authenticated peer connection per exporter name and redials
// on failure. The link key is looked up per name so that several keys (with
// priorities) can be supported later without changing callers.
type pool struct {
	node     *peer.Node
	linkKey  string
	ctx      context.Context
	mu       sync.Mutex
	ents     map[string]*entry
	resuming map[string]bool // session tokens with a resume loop running
}

type entry struct {
	mu          sync.Mutex
	conn        *quic.Conn
	defaultPort int
	udp         *tunnel.UDPMux // lazily created for udpConn
	udpConn     *quic.Conn
}

func (p *pool) keyFor(name string) string { return p.linkKey }

// get returns a live, link-key-authenticated connection to exporter name and
// the exporter's default target port.
func (p *pool) get(ctx context.Context, name string) (*quic.Conn, int, error) {
	p.mu.Lock()
	e := p.ents[name]
	if e == nil {
		e = &entry{}
		p.ents[name] = e
	}
	p.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil && e.conn.Context().Err() == nil {
		return e.conn, e.defaultPort, nil
	}
	key := p.keyFor(name)
	info, err := p.node.Lookup(ctx, key, name)
	if err != nil {
		return nil, 0, err
	}
	conn, via, err := p.node.DialPeer(ctx, info)
	if err != nil {
		return nil, 0, err
	}
	port, err := peer.AuthenticateAsClient(ctx, conn, key)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", name, err)
	}
	log.Printf("connected to %q via %s", name, via)
	e.conn, e.defaultPort = conn, port
	return conn, port, nil
}

// udpMux returns the UDP multiplexer for the live connection to name.
func (p *pool) udpMux(ctx context.Context, name string) (*tunnel.UDPMux, error) {
	conn, _, err := p.get(ctx, name)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	e := p.ents[name]
	p.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.udp == nil || e.udpConn != conn {
		e.udp = tunnel.NewUDPMux(conn)
		e.udpConn = conn
	}
	return e.udp, nil
}

// dropAll closes every peer connection so the next use redials; called when
// the local network changes (roaming) because the old paths are dead anyway.
func (p *pool) dropAll(reason string) {
	p.mu.Lock()
	ents := make([]*entry, 0, len(p.ents))
	for _, e := range p.ents {
		ents = append(ents, e)
	}
	p.mu.Unlock()
	for _, e := range ents {
		e.mu.Lock()
		if e.conn != nil && e.conn.Context().Err() == nil {
			log.Printf("dropping connection: %s", reason)
			e.conn.CloseWithError(0, reason)
		}
		e.mu.Unlock()
	}
	p.node.DropControl()
}

// watchNetwork polls the local address set and drops connections when an
// address disappears, so roaming between networks recovers in seconds
// instead of waiting for the idle timeout. Added addresses are ignored: they
// cannot break an existing path, and after a network change IPv6 addresses
// tend to arrive one by one for several seconds, which must not cause a
// redial each time. A removal has to persist for one extra tick before it
// counts, to ride out brief flaps.
func (p *pool) watchNetwork(ctx context.Context) {
	known := addrSet(netutil.LocalAddrs(0))
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := addrSet(netutil.LocalAddrs(0))
		lost := false
		for a := range known {
			if !cur[a] {
				lost = true
				break
			}
		}
		switch {
		case lost && pending:
			known = cur
			pending = false
			p.dropAll("local network changed")
		case lost:
			pending = true // confirm on the next tick
		default:
			pending = false
			for a := range cur {
				known[a] = true
			}
		}
	}
}

func addrSet(addrs []string) map[string]bool {
	m := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		m[a] = true
	}
	return m
}

// open returns a resumable session to target on exporter name. If the
// tunnel is lost, the session reconnects on its own for up to resume.Grace.
func (p *pool) open(ctx context.Context, name, target string) (*resume.Session, error) {
	conn, _, err := p.get(ctx, name)
	if err != nil {
		return nil, err
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = "-"
	}
	sess := resume.New(resume.NewToken())
	rd, _, err := tunnel.Open(st, fmt.Sprintf("CONNECT %s %s", target, sess.Token))
	if err != nil {
		st.CancelRead(0)
		st.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	sess.OnDetach = func(s *resume.Session) { p.resumeSession(name, s) }
	if err := sess.Attach(st, rd, 0); err != nil {
		return nil, err
	}
	return sess, nil
}

// resumeSession re-attaches s over a (re)dialed connection, retrying until
// resume.Grace has passed.
func (p *pool) resumeSession(name string, s *resume.Session) {
	// One loop per session: a detach during a resume attempt just makes the
	// running loop retry, instead of racing a second loop against it.
	p.mu.Lock()
	if p.resuming[s.Token] {
		p.mu.Unlock()
		return
	}
	p.resuming[s.Token] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.resuming, s.Token)
		p.mu.Unlock()
	}()
	deadline := time.Now().Add(resume.Grace)
	backoff := 500 * time.Millisecond
	for time.Now().Before(deadline) && !s.Closed() && p.ctx.Err() == nil {
		if err := p.tryResume(name, s); err == nil {
			if detached, _ := s.Detached(); !detached {
				log.Printf("session %s to %q resumed", s.Token[:8], name)
				return
			}
			// detached again while we were attaching: go round once more
		} else if p.node.Verbose {
			log.Printf("resume %s: %v", s.Token[:8], err)
		}
		time.Sleep(backoff)
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
	log.Printf("session %s to %q could not be resumed", s.Token[:8], name)
	s.Close()
}

func (p *pool) tryResume(name string, s *resume.Session) error {
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	conn, _, err := p.get(ctx, name)
	if err != nil {
		return err
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	rd, reply, err := tunnel.Open(st, fmt.Sprintf("RESUME %s %d", s.Token, s.Recv()))
	if err != nil {
		st.CancelRead(0)
		st.Close()
		return err
	}
	peerRecv, err := strconv.ParseUint(reply, 10, 64)
	if err != nil {
		st.CancelRead(0)
		st.Close()
		return fmt.Errorf("bad resume reply %q", reply)
	}
	return s.Attach(st, rd, peerRecv)
}

// DefaultServer is the public rendezvous server used when -s / $MSNW_SERVER
// is not given, so a downloaded binary works without running a server.
const DefaultServer = "relay.ikeji.ma:4433"

// nodeFlags are the connection options shared by client-side subcommands.
type nodeFlags struct {
	server, linkKey, serverKey, serverFP *string
	port                                 *int
	verbose                              *bool
	logFile                              *string
}

func addNodeFlags(fs *flag.FlagSet) *nodeFlags {
	return &nodeFlags{
		server:    fs.String("s", envOr("MSNW_SERVER", DefaultServer), "rendezvous server host:port (or $MSNW_SERVER)"),
		linkKey:   fs.String("key", os.Getenv("MSNW_KEY"), "link key shared with the exporter (or $MSNW_KEY); required"),
		serverKey: fs.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key (or $MSNW_SERVER_KEY), if the server requires one"),
		serverFP:  fs.String("server-fp", os.Getenv("MSNW_SERVER_FP"), "pin the server's sha256 fingerprint (or $MSNW_SERVER_FP)"),
		port:      fs.Int("port", 0, "local UDP port to bind (0 = random)"),
		verbose:   fs.Bool("v", false, "verbose logging"),
		logFile:   fs.String("log", os.Getenv("MSNW_LOG"), "append log output to this file instead of stderr (or $MSNW_LOG)"),
	}
}

// parseWithTrailingFlags parses args allowing flags after positional
// arguments too ("msnw mosh user@home -v"), which the flag package does not
// do on its own. It returns the positional arguments.
func parseWithTrailingFlags(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return pos
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// exportEnv puts the effective connection options into the environment so
// that child processes (ssh's ProxyCommand running "msnw client") inherit
// them without putting the key on a command line.
func (nf *nodeFlags) exportEnv() {
	os.Setenv("MSNW_SERVER", *nf.server)
	os.Setenv("MSNW_KEY", *nf.linkKey)
	os.Setenv("MSNW_SERVER_KEY", *nf.serverKey)
	os.Setenv("MSNW_SERVER_FP", *nf.serverFP)
}

// newPool binds the shared socket, connects the node and starts the network
// watcher. The returned stop function releases everything.
func newPool(nf *nodeFlags) (*pool, func(), error) {
	quietQUIC()
	if *nf.logFile != "" {
		f, err := os.OpenFile(*nf.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		log.SetOutput(f)
	}
	id, err := ident.New()
	if err != nil {
		return nil, nil, err
	}
	node, err := peer.New(id, *nf.server, *nf.serverKey, *nf.serverFP, *nf.port)
	if err != nil {
		return nil, nil, err
	}
	node.Verbose = *nf.verbose
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	p := &pool{node: node, linkKey: *nf.linkKey, ctx: ctx, ents: map[string]*entry{}, resuming: map[string]bool{}}
	go p.watchNetwork(ctx)
	return p, func() { stop(); node.Close() }, nil
}

// quietQUIC silences quic-go's receive-buffer warning; it is harmless for a
// tunnel of this size, and sysctl advice lives in the README.
func quietQUIC() {
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	}
}

// Client reaches services published by msnw export.
func Client(args []string) {
	args = netutil.OptionalValueFlag(args, "l", "auto")
	args = netutil.OptionalValueFlag(args, "socks5", "127.0.0.1:1080")
	fs := flag.NewFlagSet("msnw client", flag.ExitOnError)
	name := fs.String("n", "", "exporter NAME[:port|:host:port] to connect to (default exporter in socks5 mode)")
	listen := fs.String("l", "", "listen locally (port, :port, host:port, or udp:port; bare -l uses the exporter's port)")
	socks := fs.String("socks5", "", "run a SOCKS5 proxy (bare --socks5 listens on 127.0.0.1:1080)")
	nf := addNodeFlags(fs)
	fs.Parse(args)

	if *nf.linkKey == "" || (*name == "" && *socks == "") {
		fmt.Fprintln(os.Stderr, "usage: msnw client -key LINKKEY -n NAME[:port] [-l [addr]] | --socks5 [addr] [-n NAME]   (-s server, -server-key K)")
		os.Exit(2)
	}
	log.SetOutput(os.Stderr)
	p, stop, err := newPool(nf)
	if err != nil {
		log.Fatal(err)
	}
	defer stop()
	ctx := p.ctx

	switch {
	case *socks != "":
		runSocks(ctx, p, *socks, *name)
	case *listen != "":
		runListen(ctx, p, *name, *listen)
	default:
		runStdio(ctx, p, *name)
	}
}

func runStdio(ctx context.Context, p *pool, spec string) {
	name, target := netutil.SplitName(spec)
	sess, err := p.open(ctx, name, target)
	if err != nil {
		log.Fatal(err)
	}
	tunnel.PipeRW(sess, os.Stdin, os.Stdout)
}

func runListen(ctx context.Context, p *pool, spec, listen string) {
	name, target := netutil.SplitName(spec)
	udp := false
	if strings.HasPrefix(listen, "udp:") {
		udp = true
		listen = strings.TrimPrefix(listen, "udp:")
		if listen == "" {
			listen = "auto"
		}
	}
	// Connect eagerly: it validates the name and tells us the default port.
	_, defaultPort, err := p.get(ctx, name)
	if err != nil {
		log.Fatal(err)
	}
	if listen == "auto" {
		port := defaultPort
		if target != "" {
			_, ps, err := net.SplitHostPort(target)
			if err != nil {
				ps = target
			}
			port, _ = strconv.Atoi(ps)
		}
		if port == 0 {
			log.Fatal("-l without a value, but the exporter has no default port; give -l PORT")
		}
		listen = strconv.Itoa(port)
	}
	addr, err := netutil.ParseListen(listen)
	if err != nil {
		log.Fatal(err)
	}
	if udp {
		runListenUDP(ctx, p, name, target, addr)
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	log.Printf("listening on %s -> %s", ln.Addr(), spec)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			sess, err := p.open(ctx, name, target)
			if err != nil {
				log.Printf("%s: %v", c.RemoteAddr(), err)
				c.Close()
				return
			}
			tunnel.PipeConns(sess, c)
		}()
	}
}

const udpIdle = 10 * time.Minute

// runListenUDP forwards datagrams arriving on addr to target on exporter
// name, one flow per local source address.
func runListenUDP(ctx context.Context, p *pool, name, target, addr string) {
	uc, err := listenUDP(addr)
	if err != nil {
		log.Fatal(err)
	}
	serveUDPForward(ctx, p, name, target, uc)
}

func listenUDP(addr string) (*net.UDPConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp", ua)
}

// serveUDPForward forwards datagrams arriving on uc to target on exporter
// name, one flow per local source address, until ctx ends.
func serveUDPForward(ctx context.Context, p *pool, name, target string, uc *net.UDPConn) {
	go func() { <-ctx.Done(); uc.Close() }()
	log.Printf("listening on udp %s -> %s:%s", uc.LocalAddr(), name, target)

	var mu sync.Mutex
	flows := map[string]*tunnel.UDPFlow{}
	go func() { // reap idle flows
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			mu.Lock()
			for k, f := range flows {
				if f.Idle() > udpIdle {
					f.Close()
					delete(flows, k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, src, err := uc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		key := src.String()
		mu.Lock()
		f := flows[key]
		if f != nil {
			select {
			case <-f.Done():
				f = nil
			default:
			}
		}
		mu.Unlock()
		if f == nil {
			mux, err := p.udpMux(ctx, name)
			if err != nil {
				log.Printf("udp %s: %v", key, err)
				continue
			}
			octx, cancel := context.WithTimeout(ctx, 15*time.Second)
			dst := src
			f, err = mux.Open(octx, target, func(pl []byte) { uc.WriteToUDP(pl, dst) })
			cancel()
			if err != nil {
				log.Printf("udp %s: %v", key, err)
				continue
			}
			mu.Lock()
			flows[key] = f
			mu.Unlock()
		}
		if err := f.Send(buf[:n]); err != nil {
			log.Printf("udp %s: %v", key, err)
			f.Close()
		}
	}
}

// resolveHost maps a SOCKS destination to (exporter, target). An empty
// exporter name means "connect directly from this machine" to target.
//
//	NAME              -> exporter NAME, "~port": the port is only a hint, so a
//	                     single-target exporter (a service) ignores it
//	NAME.msnw         -> same
//	host.NAME.msnw    -> exporter NAME, target host:port (needs --all or an exact -t)
//	anything else     -> the default exporter (-n) if given, otherwise direct
//
// "localhost" is never taken for an exporter name.
func resolveHost(host string, port int, def string) (name, target string) {
	ps := strconv.Itoa(port)
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if strings.HasSuffix(h, ".msnw") {
		h = strings.TrimSuffix(h, ".msnw")
		if i := strings.LastIndex(h, "."); i >= 0 {
			return h[i+1:], net.JoinHostPort(h[:i], ps)
		}
		return h, "~" + ps
	}
	if !strings.Contains(h, ".") && net.ParseIP(h) == nil && h != "localhost" {
		return h, "~" + ps
	}
	return def, net.JoinHostPort(host, ps)
}

func runSocks(ctx context.Context, p *pool, listen, def string) {
	addr, err := netutil.ParseListen(listen)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	if def != "" {
		log.Printf("socks5 proxy on %s (other hosts go through exporter %q)", ln.Addr(), def)
	} else {
		log.Printf("socks5 proxy on %s (other hosts are reached directly)", ln.Addr())
	}
	direct := &net.Dialer{Timeout: 30 * time.Second}
	dial := func(ctx context.Context, host string, port int) (net.Conn, error) {
		name, target := resolveHost(host, port, def)
		if name == "" { // not an msnw name and no default exporter
			return direct.DialContext(ctx, "tcp", target)
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return p.open(dctx, name, target)
	}
	socks5.Serve(ctx, ln, dial, log.Printf)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
