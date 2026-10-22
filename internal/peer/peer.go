// Package peer implements the node side of msnw: one UDP socket shared by the
// control connection to the rendezvous server and by all direct peer-to-peer
// QUIC connections, so that the NAT mapping the server observes is the same
// one peers punch through. Relayed connections get a socket of their own
// each, because the relay tells sessions apart by source address.
package peer

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ikeji/mysmallnetwork/internal/buildinfo"
	"github.com/ikeji/mysmallnetwork/internal/ident"
	"github.com/ikeji/mysmallnetwork/internal/netutil"
	"github.com/ikeji/mysmallnetwork/internal/proto"
)

// Node owns the shared transport and one control connection per rendezvous
// server. Every server is used at the same time: an exporter registers with
// all of them and an importer asks all of them, so one server being down
// costs nothing but a log line.
type Node struct {
	Ident     *ident.Identity
	Transport *quic.Transport
	ServerKey string // optional gate for the rendezvous servers
	Verbose   bool

	servers []*server
	pins    map[string]bool // accepted server fingerprints; empty = not verified
}

// server is one rendezvous server and its control connection.
type server struct {
	addr string // host:port as configured

	mu   sync.Mutex
	ip   *net.UDPAddr // last resolution of addr (nil until it succeeds)
	ctrl *quic.Conn
}

// PeerInfo is what the server tells a client about an exporter.
type PeerInfo struct {
	Name        string
	Session     string
	Fingerprint string
	Candidates  []string
	Relay       *net.UDPAddr
}

// New binds the shared UDP socket and resolves the servers. A server whose
// name does not resolve is kept and resolved again on every dial; only when
// none of them resolves is that an error.
func New(id *ident.Identity, servers []string, serverKey string, serverFPs []string, port int) (*Node, error) {
	if len(servers) == 0 {
		return nil, errors.New("no rendezvous server configured")
	}
	n := &Node{Ident: id, ServerKey: serverKey, pins: map[string]bool{}}
	for _, fp := range serverFPs {
		if fp = ident.NormalizeFP(fp); fp != "" {
			n.pins[fp] = true
		}
	}
	var firstErr error
	resolved := 0
	for _, a := range servers {
		s := &server{addr: a}
		if ip, err := net.ResolveUDPAddr("udp", a); err == nil {
			s.ip = ip
			resolved++
		} else {
			log.Printf("server %s: %v (will retry)", a, err)
			if firstErr == nil {
				firstErr = err
			}
		}
		n.servers = append(n.servers, s)
	}
	if resolved == 0 {
		return nil, fmt.Errorf("server address: %w", firstErr)
	}
	uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	n.Transport = &quic.Transport{Conn: uc}
	return n, nil
}

// Servers lists the configured server addresses, in order.
func (n *Node) Servers() []string {
	out := make([]string, len(n.servers))
	for i, s := range n.servers {
		out[i] = s.addr
	}
	return out
}

func (n *Node) find(addr string) *server {
	for _, s := range n.servers {
		if s.addr == addr {
			return s
		}
	}
	return nil
}

func (n *Node) logf(format string, args ...any) {
	if n.Verbose {
		log.Printf(format, args...)
	}
}

// Port is the local UDP port of the shared socket.
func (n *Node) Port() int { return n.Transport.Conn.LocalAddr().(*net.UDPAddr).Port }

// Close releases the socket.
func (n *Node) Close() error { return n.Transport.Close() }

// LocalCandidates are this host's LAN addresses on the shared port.
func (n *Node) LocalCandidates() []string { return netutil.LocalAddrs(n.Port()) }

// RouteSource is the local address the kernel would use to reach a server
// right now ("" if there is no route). It is a cheap probe (a connected UDP
// socket sends nothing) and the most reliable sign of a network change: it
// flips when the default network moves, including on Android where listing
// interfaces is not permitted and a new network appears without the old one
// going away first. The first server with a known address is probed; no DNS
// is done here.
func (n *Node) RouteSource() string {
	for _, s := range n.servers {
		s.mu.Lock()
		ip := s.ip
		s.mu.Unlock()
		if ip == nil {
			continue
		}
		c, err := net.DialUDP("udp", nil, ip)
		if err != nil {
			continue
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return ""
}

// WatchRoute polls RouteSource every 2 seconds and calls onChange when it
// moved to a different address and stayed there for a second tick. Losing
// the route altogether is not reported; getting it back on another address
// is.
func (n *Node) WatchRoute(ctx context.Context, onChange func(from, to string)) {
	route := n.RouteSource()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := n.RouteSource()
		switch {
		case cur == "" || cur == route:
			pending = false
		case pending:
			from := route
			route, pending = cur, false
			onChange(from, cur)
		case route == "":
			route = cur // first route ever: nothing to drop
		default:
			pending = true // confirm on the next tick
		}
	}
}

func ctrlConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:       45 * time.Second,
		KeepAlivePeriod:      15 * time.Second,
		HandshakeIdleTimeout: 10 * time.Second,
		InitialPacketSize:    proto.InitialPacketSize,
	}
}

func peerConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:       20 * time.Second,
		KeepAlivePeriod:      5 * time.Second,
		HandshakeIdleTimeout: 6 * time.Second,
		MaxIncomingStreams:   4096,
		EnableDatagrams:      true,
		InitialPacketSize:    proto.InitialPacketSize,
	}
}

// serverTLS is the TLS config for dialing a server: pinned to the accepted
// fingerprints when any are configured, otherwise unverified (the server key
// still authenticates us, and a fake server can only refuse to introduce).
func (n *Node) serverTLS() *tls.Config {
	conf := n.Ident.ClientConfig("")
	if len(n.pins) > 0 {
		conf.VerifyPeerCertificate = ident.VerifyPinned(func(fp string) bool { return n.pins[fp] })
	}
	return conf
}

// control returns a live control connection to s, dialing (and resolving
// the name again) if needed.
func (n *Node) control(ctx context.Context, s *server) (*quic.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl != nil && s.ctrl.Context().Err() == nil {
		return s.ctrl, nil
	}
	ip, err := net.ResolveUDPAddr("udp", s.addr)
	if err != nil {
		if s.ip == nil {
			return nil, fmt.Errorf("resolve %s: %w", s.addr, err)
		}
		ip = s.ip // DNS hiccup: keep the last known address
	}
	s.ip = ip
	conn, err := n.Transport.Dial(ctx, ip, n.serverTLS(), ctrlConfig())
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.addr, err)
	}
	s.ctrl = conn
	return conn, nil
}

// dropControl forgets s's control connection so the next call redials.
func (n *Node) dropControl(s *server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl != nil {
		s.ctrl.CloseWithError(0, "reconnect")
		s.ctrl = nil
	}
}

// DropControl forgets every control connection so the next calls redial.
func (n *Node) DropControl() {
	for _, s := range n.servers {
		n.dropControl(s)
	}
}

func (n *Node) authFor(conn *quic.Conn) (string, error) {
	return ident.AuthTag(n.ServerKey, ident.LabelServer, conn.ConnectionState().TLS)
}

// relayAddr is the relay of the server behind ctrl: the address the control
// connection went to, on the port the server advertised.
func relayAddr(ctrl *quic.Conn, port int) *net.UDPAddr {
	ra := ctrl.RemoteAddr().(*net.UDPAddr)
	return &net.UDPAddr{IP: ra.IP, Port: port, Zone: ra.Zone}
}

// Punch sends hole-punching packets to every candidate until ctx ends or
// the duration passes.
func (n *Node) Punch(ctx context.Context, session string, candidates []string, d time.Duration) {
	var addrs []*net.UDPAddr
	for _, c := range candidates {
		if a, err := net.ResolveUDPAddr("udp", c); err == nil {
			addrs = append(addrs, a)
		}
	}
	pkt := proto.PunchPacket(session)
	deadline := time.Now().Add(d)
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(deadline) {
		for _, a := range addrs {
			n.Transport.WriteTo(pkt, a)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// newRelayTransport binds a socket for a single relay session. The relay
// knows a session only by the source address of its packets, so two sessions
// sent from one socket would take over each other's binding: the newer one
// wins and the older connection stops receiving.
func newRelayTransport() (*quic.Transport, error) {
	uc, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	return &quic.Transport{Conn: uc}, nil
}

// closeRelayTransport releases a transport from newRelayTransport
// (Transport.Close leaves a socket it did not create open).
func closeRelayTransport(tr *quic.Transport) {
	tr.Close()
	tr.Conn.Close()
}

// relayHello binds tr's address to session on the relay (sent a few times
// because it is plain UDP).
func relayHello(ctx context.Context, tr *quic.Transport, relay *net.UDPAddr, session string, role byte) {
	pkt := proto.RelayHello(session, role)
	for i := 0; i < 4; i++ {
		tr.WriteTo(pkt, relay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// ---- client side -----------------------------------------------------------

// Lookup asks every server at once how to reach exporter name within the
// namespace of linkKey and returns the first answer; a server that is down
// only adds to the error when none of them knows the name. The servers only
// see HashName(linkKey, name).
func (n *Node) Lookup(ctx context.Context, linkKey, name string) (*PeerInfo, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		info *PeerInfo
		err  error
	}
	ch := make(chan result, len(n.servers))
	for _, s := range n.servers {
		go func(s *server) {
			info, err := n.lookupAt(ctx, s, linkKey, name)
			if err != nil {
				err = fmt.Errorf("%s: %w", s.addr, err)
			}
			ch <- result{info, err}
		}(s)
	}
	var errs []string
	for range n.servers {
		r := <-ch
		if r.err == nil {
			return r.info, nil
		}
		n.logf("lookup %q: %v", name, r.err)
		errs = append(errs, r.err.Error())
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// lookupAt runs one lookup over s's control connection. Cancelling ctx
// (because another server answered first) aborts it without touching a
// healthy control connection.
func (n *Node) lookupAt(ctx context.Context, s *server, linkKey, name string) (*PeerInfo, error) {
	ctrl, err := n.control(ctx, s)
	if err != nil {
		return nil, err
	}
	auth, err := n.authFor(ctrl)
	if err != nil {
		return nil, err
	}
	st, err := ctrl.OpenStreamSync(ctx)
	if err != nil {
		if ctx.Err() == nil {
			n.dropControl(s)
		}
		return nil, err
	}
	defer st.Close()
	done := make(chan struct{})
	defer close(done)
	go func() { // a cancelled lookup must not sit in Read for its deadline
		select {
		case <-ctx.Done():
			st.CancelRead(0)
			st.CancelWrite(0)
		case <-done:
		}
	}()
	err = proto.Write(st, &proto.Message{
		Type:        proto.TypeConnect,
		Version:     buildinfo.Version(),
		Name:        ident.HashName(linkKey, name),
		Auth:        auth,
		Fingerprint: n.Ident.Fingerprint,
		LocalAddrs:  n.LocalCandidates(),
	})
	if err != nil {
		return nil, err
	}
	st.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := proto.Read(st)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		n.dropControl(s)
		return nil, fmt.Errorf("server: %w", err)
	}
	switch m.Type {
	case proto.TypePeer:
		return &PeerInfo{
			Name:        name,
			Session:     m.Session,
			Fingerprint: m.PeerFingerprint,
			Candidates:  m.Candidates,
			Relay:       relayAddr(ctrl, m.RelayPort),
		}, nil
	case proto.TypeError:
		return nil, errors.New(m.Error + buildinfo.Mismatch("server", m.Version, "importer"))
	default:
		return nil, fmt.Errorf("unexpected reply %q%s", m.Type, buildinfo.Mismatch("server", m.Version, "importer"))
	}
}

type dialResult struct {
	conn  *quic.Conn
	via   string
	relay bool
	err   error
}

// DialPeer races direct connections to every candidate while punching, and
// falls back to the relay shortly after. The first successful QUIC handshake
// wins; the peer's certificate must match the pinned fingerprint.
func (n *Node) DialPeer(ctx context.Context, info *PeerInfo) (*quic.Conn, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// The relay path gets its own socket (see newRelayTransport); without one
	// we fall back to the shared socket, which works for a single session.
	relayTr, err := newRelayTransport()
	if err != nil {
		n.logf("relay socket: %v", err)
		relayTr = n.Transport
	}

	go n.Punch(ctx, info.Session, info.Candidates, 10*time.Second)
	go relayHello(ctx, relayTr, info.Relay, info.Session, proto.RoleClient)

	tlsConf := n.Ident.ClientConfig(info.Fingerprint)
	results := make(chan dialResult, len(info.Candidates)+maxLearned+1)
	var mu sync.Mutex
	attempts := 0
	tried := map[string]bool{}
	dial := func(tr *quic.Transport, addrStr, via string, delay time.Duration) {
		mu.Lock()
		if tried[addrStr] || attempts >= cap(results) {
			mu.Unlock()
			return
		}
		tried[addrStr] = true
		attempts++
		mu.Unlock()
		go func() {
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					results <- dialResult{err: ctx.Err()}
					return
				}
			}
			addr, err := net.ResolveUDPAddr("udp", addrStr)
			if err != nil {
				results <- dialResult{err: err}
				return
			}
			conn, err := tr.Dial(ctx, addr, tlsConf, peerConfig())
			if err != nil {
				n.logf("dial %s: %v", via, err)
			}
			results <- dialResult{conn: conn, via: via, relay: tr == relayTr, err: err}
		}()
	}
	relayDelay := 1500 * time.Millisecond
	forceRelay := os.Getenv("MSNW_FORCE_RELAY") != "" // debugging aid: skip direct paths
	if forceRelay {
		relayDelay = 0
	} else {
		for _, c := range info.Candidates {
			dial(n.Transport, c, "direct "+c, 0)
		}
	}
	dial(relayTr, info.Relay.String(), "relay "+info.Relay.String(), relayDelay)

	// A punch from the exporter may arrive from an address the server never
	// saw (e.g. the exporter sits behind a symmetric NAT and we are reachable
	// anyway). Dial whatever punches us.
	if !forceRelay {
		seen := map[string]bool{}
		go n.learnFromPunches(ctx, info.Session, func(addr string) {
			if seen[addr] { // punches repeat every few hundred ms
				return
			}
			seen[addr] = true
			n.logf("learned candidate %s from punch", addr)
			dial(n.Transport, addr, "direct "+addr+" (learned)", 0)
		})
	}

	var winner *quic.Conn
	var via string
	var viaRelay bool
	var firstErr error
	for i := 0; ; i++ {
		mu.Lock()
		done := i >= attempts
		mu.Unlock()
		if done {
			break
		}
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if winner == nil {
			winner, via, viaRelay = r.conn, r.via, r.relay
			cancel() // abort the remaining handshakes
		} else {
			r.conn.CloseWithError(0, "lost the race")
		}
	}
	if relayTr != n.Transport {
		if viaRelay {
			go func() { // the socket lives as long as the connection
				<-winner.Context().Done()
				closeRelayTransport(relayTr)
			}()
		} else {
			closeRelayTransport(relayTr)
		}
	}
	if winner == nil {
		return nil, "", fmt.Errorf("could not reach %s: %w", info.Name, firstErr)
	}
	return winner, via, nil
}

const maxLearned = 8

// learnFromPunches reports source addresses of punch packets carrying our
// session id until ctx ends.
func (n *Node) learnFromPunches(ctx context.Context, session string, found func(addr string)) {
	buf := make([]byte, 2048)
	for {
		k, from, err := n.Transport.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if s, ok := proto.ParsePunch(buf[:k]); ok && s == session {
			found(from.String())
		}
	}
}

// ---- exporter side ---------------------------------------------------------

// Register announces HashName(linkKey, name) to the server at addr (one of
// Servers) and blocks, invoking onIncoming for every client introduction
// with the relay address of that server, until the control stream breaks.
func (n *Node) Register(ctx context.Context, addr, linkKey, name string, onIncoming func(m *proto.Message, relay *net.UDPAddr)) error {
	s := n.find(addr)
	if s == nil {
		return fmt.Errorf("unknown server %s", addr)
	}
	ctrl, err := n.control(ctx, s)
	if err != nil {
		return err
	}
	auth, err := n.authFor(ctrl)
	if err != nil {
		return err
	}
	st, err := ctrl.OpenStreamSync(ctx)
	if err != nil {
		n.dropControl(s)
		return err
	}
	defer st.Close()
	err = proto.Write(st, &proto.Message{
		Type:        proto.TypeRegister,
		Version:     buildinfo.Version(),
		Name:        ident.HashName(linkKey, name),
		Auth:        auth,
		Fingerprint: n.Ident.Fingerprint,
		LocalAddrs:  n.LocalCandidates(),
	})
	if err != nil {
		return err
	}
	st.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := proto.Read(st)
	if err != nil {
		n.dropControl(s)
		return fmt.Errorf("server: %w", err)
	}
	st.SetReadDeadline(time.Time{})
	if m.Type != proto.TypeOK {
		return fmt.Errorf("register rejected: %s%s", m.Error, buildinfo.Mismatch("server", m.Version, "exporter"))
	}
	log.Printf("registered %q at %s (public %s)%s", name, s.addr, m.Reflexive, buildinfo.Mismatch("server", m.Version, "exporter"))

	// The blocking Read below does not observe ctx; unblock it on shutdown.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			st.CancelRead(0)
			n.dropControl(s)
		case <-done:
		}
	}()

	for {
		m, err := proto.Read(st)
		if err != nil {
			n.dropControl(s)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("control stream: %w", err)
		}
		switch m.Type {
		case proto.TypeIncoming:
			onIncoming(m, relayAddr(ctrl, m.RelayPort))
		case proto.TypeError:
			return errors.New(m.Error)
		}
	}
}

// Listen accepts peer connections from clients whose fingerprint allow()
// admits.
func (n *Node) Listen(allow func(fp string) bool) (*quic.Listener, error) {
	return n.Transport.Listen(n.Ident.ServerConfig(allow), peerConfig())
}

// relayAcceptWindow is how long the exporter keeps a relay socket waiting for
// the client; DialPeer gives up after 20 seconds.
const relayAcceptWindow = 30 * time.Second

// AcceptRelay binds a dedicated socket to session on the relay (see
// newRelayTransport) and returns the client's connection if it arrives that
// way. It fails with a deadline error when the client got through directly
// or not at all. The socket is released when the connection ends.
func (n *Node) AcceptRelay(ctx context.Context, relay *net.UDPAddr, session string, allow func(fp string) bool) (*quic.Conn, error) {
	tr, err := newRelayTransport()
	if err != nil {
		return nil, err
	}
	ln, err := tr.Listen(n.Ident.ServerConfig(allow), peerConfig())
	if err != nil {
		closeRelayTransport(tr)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, relayAcceptWindow)
	defer cancel()
	go relayHello(ctx, tr, relay, session, proto.RoleExporter)
	conn, err := ln.Accept(ctx)
	ln.Close() // one connection per session; an accepted one is not affected
	if err != nil {
		closeRelayTransport(tr)
		return nil, err
	}
	go func() {
		<-conn.Context().Done()
		closeRelayTransport(tr)
	}()
	return conn, nil
}

// ---- peer authentication (link key) ----------------------------------------

const authTimeout = 10 * time.Second

// AuthenticateAsClient runs the link-key handshake on a freshly dialed peer
// connection and returns the exporter's default target port and version
// ("" for builds that predate version exchange). On failure the connection
// is closed.
func AuthenticateAsClient(ctx context.Context, conn *quic.Conn, linkKey string) (int, string, error) {
	fail := func(err error) (int, string, error) {
		conn.CloseWithError(1, "authentication failed")
		return 0, "", err
	}
	cs := conn.ConnectionState().TLS
	mine, err := ident.AuthTag(linkKey, ident.LabelClient, cs)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(authTimeout))
	if _, err := fmt.Fprintf(st, "AUTH %s %s\n", mine, buildinfo.Version()); err != nil {
		return fail(err)
	}
	line, err := bufio.NewReader(st).ReadString('\n')
	if err != nil {
		return fail(fmt.Errorf("exporter did not answer authentication: %w", err))
	}
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != "AUTH" {
		return fail(errors.New("exporter rejected the link key"))
	}
	port, err := strconv.Atoi(f[2])
	if err != nil {
		return fail(errors.New("exporter rejected the link key"))
	}
	peerVersion := ""
	if len(f) > 3 {
		peerVersion = f[3]
	}
	if !ident.AuthOK(linkKey, ident.LabelExporter, f[1], cs) {
		return fail(errors.New("exporter has a different link key" + buildinfo.Mismatch("exporter", peerVersion, "importer")))
	}
	return port, peerVersion, nil
}

// AuthenticateAsExporter accepts the first stream of a peer connection and
// verifies the client's link-key proof before answering with our own. It
// returns the client's version ("" for older builds). The connection is
// closed on any failure.
func AuthenticateAsExporter(ctx context.Context, conn *quic.Conn, linkKey string, defaultPort int) (string, error) {
	fail := func(err error) (string, error) {
		conn.CloseWithError(1, "authentication failed")
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	st, err := conn.AcceptStream(ctx)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(authTimeout))
	line, err := bufio.NewReader(st).ReadString('\n')
	if err != nil {
		return fail(err)
	}
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "AUTH" {
		return fail(errors.New("importer did not authenticate"))
	}
	peerVersion := ""
	if len(f) > 2 {
		peerVersion = f[2]
	}
	cs := conn.ConnectionState().TLS
	if !ident.AuthOK(linkKey, ident.LabelClient, f[1], cs) {
		return fail(errors.New("importer has a different link key" + buildinfo.Mismatch("importer", peerVersion, "exporter")))
	}
	mine, err := ident.AuthTag(linkKey, ident.LabelExporter, cs)
	if err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintf(st, "AUTH %s %d %s\n", mine, defaultPort, buildinfo.Version()); err != nil {
		return fail(err)
	}
	return peerVersion, nil
}
