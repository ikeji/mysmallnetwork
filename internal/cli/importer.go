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

	"github.com/ikeji/mysmallnetwork/internal/buildinfo"
	"github.com/ikeji/mysmallnetwork/internal/httpproxy"
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
	peerVersion string         // exporter's build version ("" if it did not say)
	udp         *tunnel.UDPMux // lazily created for udpConn
	udpConn     *quic.Conn
	users       int       // live leases: TCP sessions and UDP flows on conn
	idleSince   time.Time // when users last dropped to zero; zero while in use
}

// An unused peer connection costs a keepalive round trip every few seconds
// for as long as it lives, which adds up on a phone. Connections that have
// carried nothing for peerIdle are closed; the next use redials (a second or
// two for the lookup, the hole punch and the authentication).
const (
	peerIdle  = 2 * time.Minute
	idleSweep = 10 * time.Second
)

func (p *pool) keyFor(name string) string { return p.linkKey }

func (p *pool) entry(name string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ents[name]
	if e == nil {
		e = &entry{}
		p.ents[name] = e
	}
	return e
}

// get returns a live, link-key-authenticated connection to exporter name and
// the exporter's default target port. Nothing keeps the connection alive for
// the caller: it may be closed as idle at any time, so anything that puts a
// stream or flow on it goes through lease instead.
func (p *pool) get(ctx context.Context, name string) (*quic.Conn, int, error) {
	e := p.entry(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	return p.connectLocked(ctx, name, e)
}

// lease is get plus a hold on the connection: it is not closed as idle while
// a lease on it is live. The hold is counted per exporter, so it survives a
// redial (a resumed session keeps its original lease). release may be called
// more than once.
func (p *pool) lease(ctx context.Context, name string) (*quic.Conn, int, func(), error) {
	e := p.entry(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	conn, port, err := p.connectLocked(ctx, name, e)
	if err != nil {
		return nil, 0, nil, err
	}
	e.users++
	e.idleSince = time.Time{}
	var once sync.Once
	release := func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.users--
			if e.users == 0 {
				e.idleSince = time.Now()
			}
		})
	}
	return conn, port, release, nil
}

// connectLocked returns e's connection, dialing if it is gone. Caller holds e.mu.
func (p *pool) connectLocked(ctx context.Context, name string, e *entry) (*quic.Conn, int, error) {
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
		// Every path failed, relay included. That usually means the server's
		// view of us is stale (the address it saw on the control connection
		// is not the one our packets leave from any more, as after a network
		// change the watcher missed), so start over with a fresh control
		// connection on the next attempt rather than keep asking from the
		// old one.
		p.node.DropControl()
		return nil, 0, err
	}
	port, ver, err := peer.AuthenticateAsClient(ctx, conn, key)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", name, err)
	}
	log.Printf("connected to %q via %s%s", name, via, buildinfo.Mismatch("exporter", ver, "importer"))
	e.conn, e.defaultPort, e.peerVersion = conn, port, ver
	if e.users == 0 {
		e.idleSince = time.Now()
	}
	return conn, port, nil
}

// sweepIdle closes peer connections that have carried nothing for peerIdle.
func (p *pool) sweepIdle(ctx context.Context) {
	t := time.NewTicker(idleSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.sweepOnce(peerIdle)
	}
}

// sweepOnce closes every connection that has been idle for at least d.
func (p *pool) sweepOnce(d time.Duration) {
	p.mu.Lock()
	ents := make(map[string]*entry, len(p.ents))
	for name, e := range p.ents {
		ents[name] = e
	}
	p.mu.Unlock()
	for name, e := range ents {
		e.mu.Lock()
		live := e.conn != nil && e.conn.Context().Err() == nil
		if live && e.users == 0 && !e.idleSince.IsZero() && time.Since(e.idleSince) >= d {
			log.Printf("connection to %q closed after %s idle", name, d)
			e.conn.CloseWithError(0, "idle")
		}
		e.mu.Unlock()
	}
}

// annotate appends the exporter/client versions to an error from exporter
// name when they differ, since protocol errors usually mean a version skew.
func (p *pool) annotate(name string, err error) error {
	p.mu.Lock()
	e := p.ents[name]
	p.mu.Unlock()
	if e == nil {
		return err
	}
	e.mu.Lock()
	ver := e.peerVersion
	e.mu.Unlock()
	if note := buildinfo.Mismatch("exporter", ver, "importer"); note != "" {
		return fmt.Errorf("%w%s", err, note)
	}
	return err
}

// openUDP opens a UDP flow to target on exporter name. The flow holds the
// connection (see lease) until it ends.
func (p *pool) openUDP(ctx context.Context, name, target string, recv func([]byte)) (*tunnel.UDPFlow, error) {
	conn, _, release, err := p.lease(ctx, name)
	if err != nil {
		return nil, err
	}
	e := p.entry(name)
	e.mu.Lock()
	if e.udp == nil || e.udpConn != conn {
		e.udp = tunnel.NewUDPMux(conn)
		e.udpConn = conn
	}
	mux := e.udp
	e.mu.Unlock()
	octx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f, err := mux.Open(octx, target, recv)
	if err != nil {
		release()
		return nil, err
	}
	go func() {
		<-f.Done()
		release()
	}()
	return f, nil
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

// watchNetwork polls for a network change and drops connections when one is
// confirmed, so roaming between networks recovers in seconds instead of
// waiting for the idle timeout. Two signals are watched:
//
//   - the local address the kernel picks to reach the server (the route
//     source). It changes when the default network moves, even when the old
//     network stays up for a while, which is how phones hand over from
//     mobile data to Wi-Fi, and it works where listing interfaces is not
//     allowed (Android);
//   - a local address disappearing, for networks that simply go away.
//
// Added addresses are ignored: they cannot break an existing path, and after
// a network change IPv6 addresses tend to arrive one by one for several
// seconds, which must not cause a redial each time. A change has to persist
// for one extra tick before it counts, to ride out brief flaps.
func (p *pool) watchNetwork(ctx context.Context) {
	known := addrSet(netutil.LocalAddrs(0))
	route := p.node.RouteSource()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	pending := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := addrSet(netutil.LocalAddrs(0))
		curRoute := p.node.RouteSource()
		reason := ""
		if curRoute != "" && route != "" && curRoute != route {
			reason = fmt.Sprintf("local network changed (%s -> %s)", route, curRoute)
		} else {
			for a := range known {
				if !cur[a] {
					reason = "local network changed (" + a + " gone)"
					break
				}
			}
		}
		switch {
		case reason != "" && pending != "":
			known, route, pending = cur, curRoute, ""
			p.dropAll(reason)
		case reason != "":
			pending = reason // confirm on the next tick
		default:
			pending = ""
			if curRoute != "" {
				route = curRoute
			}
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
	conn, _, release, err := p.lease(ctx, name)
	if err != nil {
		return nil, err
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		release()
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
		release()
		return nil, p.annotate(name, fmt.Errorf("%s: %w", name, err))
	}
	sess.OnClose = func(*resume.Session) { release() }
	sess.OnDetach = func(s *resume.Session) { p.resumeSession(name, s) }
	if err := sess.Attach(st, rd, 0); err != nil {
		sess.Close() // nobody will ever read it; this also releases the lease
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
		return p.annotate(name, err)
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
// that child processes (ssh's ProxyCommand running "msnw connect") inherit
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
	go p.sweepIdle(ctx)
	return p, func() { stop(); node.Close() }, nil
}

// quietQUIC silences quic-go's receive-buffer warning; it is harmless for a
// tunnel of this size, and sysctl advice lives in the README.
func quietQUIC() {
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	}
}

// Each consumer-side command parses its own flags, builds the shared pool
// and hands over to one of the runners below.

func usage(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

func startPool(nf *nodeFlags) (*pool, func()) {
	if *nf.linkKey == "" {
		usage("a link key is required (-key or $MSNW_KEY)")
	}
	log.SetOutput(os.Stderr)
	p, stop, err := newPool(nf)
	if err != nil {
		log.Fatal(err)
	}
	return p, stop
}

// Connect pipes stdin/stdout to a published service: msnw connect NAME[:port]
func Connect(args []string) {
	fs := flag.NewFlagSet("msnw connect", flag.ExitOnError)
	nf := addNodeFlags(fs)
	pos := parseWithTrailingFlags(fs, args)
	if len(pos) != 1 {
		usage("usage: msnw connect [-key LINKKEY] NAME[:port|:host:port]")
	}
	p, stop := startPool(nf)
	defer stop()
	runStdio(p.ctx, p, pos[0])
}

// Import listens locally and forwards to a published service:
// msnw import -l [addr] NAME[:port]   (addr: port, :port, host:port or udp:port; bare -l = the exporter's port)
func Import(args []string) {
	args = netutil.OptionalValueFlag(args, "l", "auto")
	fs := flag.NewFlagSet("msnw import", flag.ExitOnError)
	listen := fs.String("l", "", "local address to listen on: port, :port, host:port or udp:port (bare -l uses the exporter's port)")
	nf := addNodeFlags(fs)
	pos := parseWithTrailingFlags(fs, args)
	if len(pos) != 1 || *listen == "" {
		usage("usage: msnw import [-key LINKKEY] -l [addr] NAME[:port|:host:port]")
	}
	p, stop := startPool(nf)
	defer stop()
	runListen(p.ctx, p, pos[0], *listen)
}

// proxyFlags are shared by the proxy commands.
func proxyFlags(fs *flag.FlagSet) (nf *nodeFlags, def *string) {
	def = fs.String("n", "", "default exporter for destinations that are not msnw names (otherwise reached directly)")
	return addNodeFlags(fs), def
}

// Socks5Proxy runs a SOCKS5 proxy: msnw socks5-proxy [addr]
func Socks5Proxy(args []string) {
	fs := flag.NewFlagSet("msnw socks5-proxy", flag.ExitOnError)
	nf, def := proxyFlags(fs)
	pos := parseWithTrailingFlags(fs, args)
	addr := "127.0.0.1:1080"
	if len(pos) > 1 {
		usage("usage: msnw socks5-proxy [-key LINKKEY] [-n DEFAULT] [addr]   (default 127.0.0.1:1080)")
	} else if len(pos) == 1 {
		addr = pos[0]
	}
	p, stop := startPool(nf)
	defer stop()
	runSocks(p.ctx, p, addr, *def)
}

// HTTPProxy runs an HTTP proxy: msnw http-proxy [addr]
func HTTPProxy(args []string) {
	fs := flag.NewFlagSet("msnw http-proxy", flag.ExitOnError)
	nf, def := proxyFlags(fs)
	pos := parseWithTrailingFlags(fs, args)
	addr := "127.0.0.1:8080"
	if len(pos) > 1 {
		usage("usage: msnw http-proxy [-key LINKKEY] [-n DEFAULT] [addr]   (default 127.0.0.1:8080)")
	} else if len(pos) == 1 {
		addr = pos[0]
	}
	p, stop := startPool(nf)
	defer stop()
	runHTTPProxy(p.ctx, p, addr, *def)
}

// Proxy runs both proxies in one process: msnw proxy [--socks5 addr] [--http addr]
func Proxy(args []string) {
	fs := flag.NewFlagSet("msnw proxy", flag.ExitOnError)
	socks := fs.String("socks5", "127.0.0.1:1080", "SOCKS5 listen address (\"\" to disable)")
	httpAddr := fs.String("http", "127.0.0.1:8080", "HTTP proxy listen address (\"\" to disable)")
	nf, def := proxyFlags(fs)
	if len(parseWithTrailingFlags(fs, args)) != 0 || (*socks == "" && *httpAddr == "") {
		usage("usage: msnw proxy [-key LINKKEY] [-n DEFAULT] [--socks5 addr] [--http addr]")
	}
	p, stop := startPool(nf)
	defer stop()
	if *socks != "" {
		go runSocks(p.ctx, p, *socks, *def)
	}
	if *httpAddr != "" {
		go runHTTPProxy(p.ctx, p, *httpAddr, *def)
	}
	<-p.ctx.Done()
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
			dst := src
			f, err = p.openUDP(ctx, name, target, func(pl []byte) { uc.WriteToUDP(pl, dst) })
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

// proxyDialer decides, per destination, between the tunnel and a direct
// connection (see resolveHost); shared by the SOCKS5 and HTTP proxies.
func proxyDialer(p *pool, def string) func(ctx context.Context, host string, port int) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 30 * time.Second}
	return func(ctx context.Context, host string, port int) (net.Conn, error) {
		name, target := resolveHost(host, port, def)
		if name == "" { // not an msnw name and no default exporter
			return direct.DialContext(ctx, "tcp", target)
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return p.open(dctx, name, target)
	}
}

func listenProxy(ctx context.Context, kind, listen, def string) net.Listener {
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
		log.Printf("%s proxy on %s (other hosts go through exporter %q)", kind, ln.Addr(), def)
	} else {
		log.Printf("%s proxy on %s (other hosts are reached directly)", kind, ln.Addr())
	}
	return ln
}

func runSocks(ctx context.Context, p *pool, listen, def string) {
	socks5.Serve(ctx, listenProxy(ctx, "socks5", listen, def), proxyDialer(p, def), log.Printf)
}

func runHTTPProxy(ctx context.Context, p *pool, listen, def string) {
	httpproxy.Serve(ctx, listenProxy(ctx, "http", listen, def), proxyDialer(p, def), log.Printf)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
