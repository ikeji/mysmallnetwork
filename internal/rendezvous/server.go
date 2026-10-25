// Package rendezvous implements the msnw server: a registry of exporters, an
// introduction service that hands each side the other's address candidates,
// and a dumb UDP relay used when hole punching fails.
package rendezvous

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/ikeji/mysmallnetwork/internal/buildinfo"
	"github.com/ikeji/mysmallnetwork/internal/ident"
	"github.com/ikeji/mysmallnetwork/internal/netutil"
	"github.com/ikeji/mysmallnetwork/internal/proto"
)

// Server is the rendezvous server.
type Server struct {
	Ident     *ident.Identity
	ServerKey string // empty = anyone may use this server
	RelayPort int

	mu        sync.Mutex
	exporters map[string]*exporter

	relay *relay
}

type exporter struct {
	name       string
	fp         string
	candidates []string // server-observed address first, then LAN addresses
	conn       *quic.Conn
	stream     *quic.Stream
	writeMu    sync.Mutex
}

// currentCandidates is candidates with the server-observed address refreshed
// from the control connection, which may have migrated since registration
// (the exporter changed networks without its control connection breaking).
func (ex *exporter) currentCandidates() []string {
	out := append([]string{ex.conn.RemoteAddr().String()}, ex.candidates...)
	return netutil.Dedup(out)
}

// udpNetwork picks "udp4" for an IPv4 listen address, 0.0.0.0 included. Go
// would otherwise open a dual-stack IPv6 socket for 0.0.0.0, on which FreeBSD
// and macOS apply the don't-fragment option to IPv6 only (see df_bsd.go), so
// "-relay 0.0.0.0:4434" is the way to get DF on IPv4 there. ":4434" stays
// dual-stack.
func udpNetwork(a *net.UDPAddr) string {
	if a.IP != nil && a.IP.To4() != nil {
		return "udp4"
	}
	return "udp"
}

// Run serves the control listener and the relay socket until ctx ends.
func (s *Server) Run(ctx context.Context, ctrlAddr, relayAddr string) error {
	s.exporters = map[string]*exporter{}

	ru, err := net.ResolveUDPAddr("udp", relayAddr)
	if err != nil {
		return err
	}
	rc, err := net.ListenUDP(udpNetwork(ru), ru)
	if err != nil {
		return fmt.Errorf("relay listen: %w", err)
	}
	defer rc.Close()
	s.RelayPort = rc.LocalAddr().(*net.UDPAddr).Port
	s.relay = newRelay(rc)
	go s.relay.run(ctx)

	cu, err := net.ResolveUDPAddr("udp", ctrlAddr)
	if err != nil {
		return err
	}
	cc, err := net.ListenUDP(udpNetwork(cu), cu)
	if err != nil {
		return fmt.Errorf("control listen: %w", err)
	}
	// A stateless reset key derived from the (possibly persistent) identity
	// lets nodes notice a restarted server on their very next packet.
	srk := quic.StatelessResetKey(sha256.Sum256(append([]byte("msnw-srk:"), s.Ident.Cert.PrivateKey.(ed25519.PrivateKey).Seed()...)))
	tr := &quic.Transport{
		Conn:              cc,
		StatelessResetKey: &srk,
		// Always validate the source address with a Retry before doing the
		// handshake, so a spoofed Initial never gets more bytes back than it
		// sent (no amplification).
		VerifySourceAddress: func(net.Addr) bool { return true },
	}
	defer tr.Close()
	ln, err := tr.Listen(s.Ident.ServerConfig(nil), &quic.Config{
		MaxIdleTimeout:    45 * time.Second,
		KeepAlivePeriod:   15 * time.Second,
		InitialPacketSize: proto.InitialPacketSize,
	})
	if err != nil {
		return err
	}
	defer ln.Close()
	var wg sync.WaitGroup
	defer func() {
		// Let every handler send its CONNECTION_CLOSE before the socket goes away.
		wg.Wait()
		time.Sleep(200 * time.Millisecond)
	}()
	log.Printf("control: listening on %s (udp)", cc.LocalAddr())
	log.Printf("relay:   listening on %s (udp)", rc.LocalAddr())
	s.relay.logBuffers()
	log.Printf("fingerprint: sha256:%s", s.Ident.Fingerprint)

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn) {
	defer conn.CloseWithError(0, "bye")
	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.handleStream(ctx, conn, st)
	}
}

func (s *Server) handleStream(ctx context.Context, conn *quic.Conn, st *quic.Stream) {
	defer st.Close()
	st.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := proto.Read(st)
	if err != nil {
		return
	}
	st.SetReadDeadline(time.Time{})
	remote := conn.RemoteAddr().String()

	if s.ServerKey != "" && !ident.AuthOK(s.ServerKey, ident.LabelServer, m.Auth, conn.ConnectionState().TLS) {
		log.Printf("%s: %s %q: bad auth", remote, m.Type, m.Name)
		proto.Write(st, &proto.Message{Type: proto.TypeError, Version: buildinfo.Version(), Error: "authentication failed"})
		return
	}
	if m.Name == "" || m.Fingerprint == "" {
		proto.Write(st, &proto.Message{Type: proto.TypeError, Version: buildinfo.Version(), Error: "name and fp required"})
		return
	}
	cands := netutil.Dedup(append([]string{remote}, m.LocalAddrs...))

	switch m.Type {
	case proto.TypeRegister:
		s.handleRegister(ctx, conn, st, m, cands, remote)
	case proto.TypeConnect:
		s.handleConnect(st, m, cands, remote)
	default:
		proto.Write(st, &proto.Message{Type: proto.TypeError, Version: buildinfo.Version(), Error: "unknown message type"})
	}
}

func (s *Server) handleRegister(ctx context.Context, conn *quic.Conn, st *quic.Stream, m *proto.Message, cands []string, remote string) {
	ex := &exporter{
		name:       m.Name,
		fp:         ident.NormalizeFP(m.Fingerprint),
		candidates: cands,
		conn:       conn,
		stream:     st,
	}
	s.mu.Lock()
	old := s.exporters[m.Name]
	s.exporters[m.Name] = ex
	s.mu.Unlock()
	if old != nil {
		log.Printf("%s: exporter %s re-registered (replacing %s)", remote, short(m.Name), old.candidates[0])
		old.send(&proto.Message{Type: proto.TypeError, Error: "replaced by a new registration"})
		old.stream.CancelRead(0)
	} else {
		log.Printf("%s: exporter %s registered (%s), candidates=%v", remote, short(m.Name), versionOf(m), cands)
	}
	if err := ex.send(&proto.Message{Type: proto.TypeOK, Version: buildinfo.Version(), Reflexive: remote}); err != nil {
		return
	}

	// Hold the stream open; the exporter never sends anything else, so a
	// read returning is our disconnect signal.
	buf := make([]byte, 16)
	for {
		if _, err := st.Read(buf); err != nil {
			break
		}
	}
	s.mu.Lock()
	if s.exporters[m.Name] == ex {
		delete(s.exporters, m.Name)
		log.Printf("%s: exporter %s gone", remote, short(m.Name))
	}
	s.mu.Unlock()
}

func (ex *exporter) send(m *proto.Message) error {
	ex.writeMu.Lock()
	defer ex.writeMu.Unlock()
	ex.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer ex.stream.SetWriteDeadline(time.Time{})
	return proto.Write(ex.stream, m)
}

func (s *Server) handleConnect(st *quic.Stream, m *proto.Message, cands []string, remote string) {
	s.mu.Lock()
	ex := s.exporters[m.Name]
	s.mu.Unlock()
	if ex == nil {
		log.Printf("%s: connect %s: no such exporter", remote, short(m.Name))
		proto.Write(st, &proto.Message{Type: proto.TypeError, Version: buildinfo.Version(), Error: "no such exporter (check the name and the link key)"})
		return
	}
	session := newSession()
	exCands := ex.currentCandidates()
	s.relay.allow(session, ipOf(remote), ipOf(exCands[0]))
	log.Printf("%s: connect %s (%s) -> %s session=%s", remote, short(m.Name), versionOf(m), exCands[0], session[:8])

	err := ex.send(&proto.Message{
		Type:            proto.TypeIncoming,
		Session:         session,
		PeerFingerprint: ident.NormalizeFP(m.Fingerprint),
		Candidates:      cands,
		RelayPort:       s.RelayPort,
	})
	if err != nil {
		proto.Write(st, &proto.Message{Type: proto.TypeError, Version: buildinfo.Version(), Error: "exporter unreachable: " + err.Error()})
		return
	}
	proto.Write(st, &proto.Message{
		Type:            proto.TypePeer,
		Version:         buildinfo.Version(),
		Session:         session,
		PeerFingerprint: ex.fp,
		Candidates:      exCands,
		RelayPort:       s.RelayPort,
	})
}

// ipOf extracts the IP from "host:port".
func ipOf(hostport string) net.IP {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil
	}
	return net.ParseIP(h)
}

func versionOf(m *proto.Message) string {
	if m.Version == "" {
		return "version unknown"
	}
	return m.Version
}

// short abbreviates a hashed name for logs.
func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

func newSession() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// ---- relay -----------------------------------------------------------------

// relay pairs two UDP endpoints per session and forwards packets between them
// verbatim. Sessions are created by handleConnect and bound by hello packets.
// A source address belongs to one session at a time (the latest it said hello
// for), so nodes use a separate socket for every session.
type relay struct {
	conn *net.UDPConn
	mu   sync.Mutex
	sess map[string]*relaySession
	addr map[string]*relaySession // "ip:port" -> session

	// Forwarding is split in two: run reads the socket and only decides where
	// a packet goes, writer does the sends. A send toward a slow uplink can
	// block, and with a single loop the kernel receive queue (often just
	// 208 KB, net.core.rmem_max) overflowed and dropped packets whenever a
	// peer on a fast first hop (same host, same data centre) burst into it.
	// QUIC then saw loss and collapsed to a few percent of the link.
	out   chan relayPacket
	pool  sync.Pool
	drops atomic.Uint64 // packets dropped because out was full
	// oversize counts packets the kernel refused to send with DF set: they
	// exceed the path MTU and the endpoints' own probing must settle lower.
	oversize atomic.Uint64
	dfErr    error // setDontFragment failure, logged at startup
}

type relayPacket struct {
	b        []byte
	n        int
	from, to *net.UDPAddr
}

// relayQueue is how many packets may wait for the writer: about 5 MB of
// 1200-byte QUIC packets, or 40 ms at 1 Gbit/s.
const relayQueue = 4096

// relaySocketBuffer is what the relay asks the kernel for on its socket. Linux
// silently clamps it to net.core.rmem_max / wmem_max; FreeBSD and macOS refuse
// anything above kern.ipc.maxsockbuf instead, which would leave the tiny
// default (42 KB on FreeBSD, under 30 packets), so setSocketBuffers halves the
// request until one is accepted. The achieved size is logged at startup so a
// too-small limit is visible.
const relaySocketBuffer = 8 << 20

func setSocketBuffers(c *net.UDPConn, size int) {
	for s := size; s >= 64<<10 && c.SetReadBuffer(s) != nil; s /= 2 {
	}
	for s := size; s >= 64<<10 && c.SetWriteBuffer(s) != nil; s /= 2 {
	}
}

// bufferHint is the sysctl that lifts the socket buffer limit on this OS.
func bufferHint() string {
	switch runtime.GOOS {
	case "freebsd", "darwin", "openbsd", "netbsd":
		return fmt.Sprintf("sysctl kern.ipc.maxsockbuf=%d", 2*relaySocketBuffer)
	default:
		return fmt.Sprintf("sysctl -w net.core.rmem_max=%d net.core.wmem_max=%d", relaySocketBuffer, relaySocketBuffer)
	}
}

type relaySession struct {
	id       string
	client   *net.UDPAddr
	exporter *net.UDPAddr
	// IPs the two parties used on their control connections. A hello is
	// only accepted from the matching IP (ports may differ behind NAT), so
	// nobody can bind a spoofed address and turn the relay into a reflector.
	clientIP, exporterIP net.IP
	created              time.Time
	lastSeen             time.Time
}

func newRelay(c *net.UDPConn) *relay {
	setSocketBuffers(c, relaySocketBuffer)
	dfErr := setDontFragment(c)
	r := &relay{conn: c, sess: map[string]*relaySession{}, addr: map[string]*relaySession{}, out: make(chan relayPacket, relayQueue)}
	r.pool.New = func() any { return make([]byte, 65535) }
	r.dfErr = dfErr
	return r
}

// logBuffers reports the socket buffer sizes the kernel actually granted.
func (r *relay) logBuffers() {
	rcv, snd, ok := socketBuffers(r.conn)
	if !ok {
		return
	}
	hint := ""
	if rcv < relaySocketBuffer/2 || snd < relaySocketBuffer/2 {
		hint = " (small; for throughput: " + bufferHint() + ")"
	}
	log.Printf("relay:   socket buffers rcv %d snd %d%s", rcv, snd, hint)
	if r.dfErr != nil {
		log.Printf("relay:   cannot set don't-fragment (%v); oversize packets will be fragmented, which some NATs reorder", r.dfErr)
	}
}

func (r *relay) allow(session string, clientIP, exporterIP net.IP) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sess[session] = &relaySession{id: session, clientIP: clientIP, exporterIP: exporterIP, created: time.Now(), lastSeen: time.Now()}
}

func (r *relay) run(ctx context.Context) {
	go r.reap(ctx)
	go r.writer(ctx)
	// recvmmsg where the platform has it: one syscall drains a whole burst,
	// which is what keeps the kernel queue from overflowing when a peer on a
	// fast link sends a GSO batch.
	batch := newBatchReader(r.conn)
	for {
		var pkts []relayPacket
		var err error
		if batch != nil {
			pkts, err = batch.read(&r.pool)
		} else {
			buf := r.pool.Get().([]byte)
			var n int
			var from *net.UDPAddr
			n, from, err = r.conn.ReadFromUDP(buf)
			if err != nil {
				r.pool.Put(buf)
			} else {
				pkts = []relayPacket{{b: buf, n: n, from: from}}
			}
		}
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		for _, p := range pkts {
			p.to = r.handle(p.b[:p.n], p.from)
			if p.to == nil {
				r.pool.Put(p.b)
				continue
			}
			select {
			case r.out <- p:
			default:
				// The writer is behind (slow uplink). Dropping here is what
				// the kernel would otherwise do, but after a much bigger queue.
				r.drops.Add(1)
				r.pool.Put(p.b)
			}
		}
	}
}

// writer sends queued packets in order; one goroutine so a session's
// packets are never reordered by the relay itself.
func (r *relay) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-r.out:
			if _, err := r.conn.WriteToUDP(p.b[:p.n], p.to); err != nil && errors.Is(err, syscall.EMSGSIZE) {
				r.oversize.Add(1)
			}
			r.pool.Put(p.b)
		}
	}
}

// handle binds hello packets and returns where a data packet goes (nil when
// it is a hello, or from an address that belongs to no session).
func (r *relay) handle(b []byte, from *net.UDPAddr) *net.UDPAddr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, role, ok := proto.ParseRelayHello(b); ok {
		rs := r.sess[session]
		if rs == nil {
			return nil
		}
		want := rs.exporterIP
		if role == proto.RoleClient {
			want = rs.clientIP
		}
		if want == nil || !want.Equal(from.IP) {
			return nil
		}
		key := from.String()
		if role == proto.RoleClient {
			if rs.client != nil {
				delete(r.addr, rs.client.String())
			}
			rs.client = from
		} else {
			if rs.exporter != nil {
				delete(r.addr, rs.exporter.String())
			}
			rs.exporter = from
		}
		r.addr[key] = rs
		rs.lastSeen = time.Now()
		return nil
	}
	rs := r.addr[from.String()]
	if rs == nil {
		return nil
	}
	var to *net.UDPAddr
	if rs.client != nil && rs.client.String() == from.String() {
		to = rs.exporter
	} else {
		to = rs.client
	}
	if to == nil {
		return nil
	}
	rs.lastSeen = time.Now()
	return to
}

func (r *relay) reap(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d := r.oversize.Swap(0); d > 0 {
			log.Printf("relay:   refused %d packets larger than the path MTU (the peers will probe lower)", d)
		}
		if d := r.drops.Swap(0); d > 0 {
			log.Printf("relay:   dropped %d packets (uplink slower than peers)", d)
		}
		r.mu.Lock()
		now := time.Now()
		for id, rs := range r.sess {
			idle := now.Sub(rs.lastSeen)
			if idle > 3*time.Minute || (rs.client == nil || rs.exporter == nil) && now.Sub(rs.created) > time.Minute {
				delete(r.sess, id)
				if rs.client != nil {
					delete(r.addr, rs.client.String())
				}
				if rs.exporter != nil {
					delete(r.addr, rs.exporter.String())
				}
			}
		}
		r.mu.Unlock()
	}
}
