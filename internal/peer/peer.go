// Package peer implements the node side of msnw: one UDP socket shared by the
// control connection to the rendezvous server and by all peer-to-peer QUIC
// connections, so that the NAT mapping the server observes is the same one
// peers punch through.
package peer

import (
	"bufio"
	"context"
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

// Node owns the shared transport and the control connection.
type Node struct {
	Ident     *ident.Identity
	Transport *quic.Transport
	Server    *net.UDPAddr
	ServerFP  string
	ServerKey string // optional gate for the rendezvous server
	Verbose   bool

	mu   sync.Mutex
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

// New resolves the server address and binds the shared UDP socket.
func New(id *ident.Identity, server, serverKey, serverFP string, port int) (*Node, error) {
	sa, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, fmt.Errorf("server address: %w", err)
	}
	uc, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	return &Node{
		Ident:     id,
		Transport: &quic.Transport{Conn: uc},
		Server:    sa,
		ServerFP:  serverFP,
		ServerKey: serverKey,
	}, nil
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

func ctrlConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:       45 * time.Second,
		KeepAlivePeriod:      15 * time.Second,
		HandshakeIdleTimeout: 10 * time.Second,
	}
}

func peerConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:       30 * time.Second,
		KeepAlivePeriod:      10 * time.Second,
		HandshakeIdleTimeout: 6 * time.Second,
		MaxIncomingStreams:   4096,
		EnableDatagrams:      true,
	}
}

// Control returns a live control connection to the server, dialing if needed.
func (n *Node) Control(ctx context.Context) (*quic.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ctrl != nil && n.ctrl.Context().Err() == nil {
		return n.ctrl, nil
	}
	conn, err := n.Transport.Dial(ctx, n.Server, n.Ident.ClientConfig(n.ServerFP), ctrlConfig())
	if err != nil {
		return nil, fmt.Errorf("dial server %s: %w", n.Server, err)
	}
	n.ctrl = conn
	return conn, nil
}

// DropControl forgets the control connection so the next call redials.
func (n *Node) DropControl() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ctrl != nil {
		n.ctrl.CloseWithError(0, "reconnect")
		n.ctrl = nil
	}
}

func (n *Node) authFor(conn *quic.Conn) (string, error) {
	return ident.AuthTag(n.ServerKey, ident.LabelServer, conn.ConnectionState().TLS)
}

// RelayAddr builds the relay address from the server host and the port the
// server advertised.
func (n *Node) RelayAddr(port int) *net.UDPAddr {
	return &net.UDPAddr{IP: n.Server.IP, Port: port, Zone: n.Server.Zone}
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

// RelayHello binds our address to session on the relay (sent a few times
// because it is plain UDP).
func (n *Node) RelayHello(ctx context.Context, relay *net.UDPAddr, session string, role byte) {
	pkt := proto.RelayHello(session, role)
	for i := 0; i < 4; i++ {
		n.Transport.WriteTo(pkt, relay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// ---- client side -----------------------------------------------------------

// Lookup asks the server how to reach exporter name within the namespace of
// linkKey. The server only sees HashName(linkKey, name).
func (n *Node) Lookup(ctx context.Context, linkKey, name string) (*PeerInfo, error) {
	ctrl, err := n.Control(ctx)
	if err != nil {
		return nil, err
	}
	auth, err := n.authFor(ctrl)
	if err != nil {
		return nil, err
	}
	st, err := ctrl.OpenStreamSync(ctx)
	if err != nil {
		n.DropControl()
		return nil, err
	}
	defer st.Close()
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
		n.DropControl()
		return nil, fmt.Errorf("server: %w", err)
	}
	switch m.Type {
	case proto.TypePeer:
		return &PeerInfo{
			Name:        name,
			Session:     m.Session,
			Fingerprint: m.PeerFingerprint,
			Candidates:  m.Candidates,
			Relay:       n.RelayAddr(m.RelayPort),
		}, nil
	case proto.TypeError:
		return nil, errors.New(m.Error + buildinfo.Mismatch("server", m.Version, "client"))
	default:
		return nil, fmt.Errorf("unexpected reply %q%s", m.Type, buildinfo.Mismatch("server", m.Version, "client"))
	}
}

type dialResult struct {
	conn *quic.Conn
	via  string
	err  error
}

// DialPeer races direct connections to every candidate while punching, and
// falls back to the relay shortly after. The first successful QUIC handshake
// wins; the peer's certificate must match the pinned fingerprint.
func (n *Node) DialPeer(ctx context.Context, info *PeerInfo) (*quic.Conn, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	go n.Punch(ctx, info.Session, info.Candidates, 10*time.Second)
	go n.RelayHello(ctx, info.Relay, info.Session, proto.RoleClient)

	tlsConf := n.Ident.ClientConfig(info.Fingerprint)
	results := make(chan dialResult, len(info.Candidates)+maxLearned+1)
	var mu sync.Mutex
	attempts := 0
	tried := map[string]bool{}
	dial := func(addrStr, via string, delay time.Duration) {
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
			conn, err := n.Transport.Dial(ctx, addr, tlsConf, peerConfig())
			if err != nil {
				n.logf("dial %s: %v", via, err)
			}
			results <- dialResult{conn: conn, via: via, err: err}
		}()
	}
	relayDelay := 1500 * time.Millisecond
	forceRelay := os.Getenv("MSNW_FORCE_RELAY") != "" // debugging aid: skip direct paths
	if forceRelay {
		relayDelay = 0
	} else {
		for _, c := range info.Candidates {
			dial(c, "direct "+c, 0)
		}
	}
	dial(info.Relay.String(), "relay "+info.Relay.String(), relayDelay)

	// A punch from the exporter may arrive from an address the server never
	// saw (e.g. the exporter sits behind a symmetric NAT and we are reachable
	// anyway). Dial whatever punches us.
	if !forceRelay {
		go n.learnFromPunches(ctx, info.Session, func(addr string) {
			n.logf("learned candidate %s from punch", addr)
			dial(addr, "direct "+addr+" (learned)", 0)
		})
	}

	var winner *quic.Conn
	var via string
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
			winner, via = r.conn, r.via
			cancel() // abort the remaining handshakes
		} else {
			r.conn.CloseWithError(0, "lost the race")
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

// Register announces HashName(linkKey, name) to the server and blocks,
// invoking onIncoming for every client introduction, until the control
// stream breaks.
func (n *Node) Register(ctx context.Context, linkKey, name string, onIncoming func(*proto.Message)) error {
	ctrl, err := n.Control(ctx)
	if err != nil {
		return err
	}
	auth, err := n.authFor(ctrl)
	if err != nil {
		return err
	}
	st, err := ctrl.OpenStreamSync(ctx)
	if err != nil {
		n.DropControl()
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
		n.DropControl()
		return fmt.Errorf("server: %w", err)
	}
	st.SetReadDeadline(time.Time{})
	if m.Type != proto.TypeOK {
		return fmt.Errorf("register rejected: %s%s", m.Error, buildinfo.Mismatch("server", m.Version, "exporter"))
	}
	log.Printf("registered %q at %s (public %s)%s", name, n.Server, m.Reflexive, buildinfo.Mismatch("server", m.Version, "exporter"))

	// The blocking Read below does not observe ctx; unblock it on shutdown.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			st.CancelRead(0)
			n.DropControl()
		case <-done:
		}
	}()

	for {
		m, err := proto.Read(st)
		if err != nil {
			n.DropControl()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("control stream: %w", err)
		}
		switch m.Type {
		case proto.TypeIncoming:
			onIncoming(m)
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
		return fail(errors.New("exporter has a different link key" + buildinfo.Mismatch("exporter", peerVersion, "client")))
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
		return fail(errors.New("client did not authenticate"))
	}
	peerVersion := ""
	if len(f) > 2 {
		peerVersion = f[2]
	}
	cs := conn.ConnectionState().TLS
	if !ident.AuthOK(linkKey, ident.LabelClient, f[1], cs) {
		return fail(errors.New("client has a different link key" + buildinfo.Mismatch("client", peerVersion, "exporter")))
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
