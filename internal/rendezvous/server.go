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
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"mysmallnetwork/internal/ident"
	"mysmallnetwork/internal/netutil"
	"mysmallnetwork/internal/proto"
)

// Server is the rendezvous server.
type Server struct {
	Ident     *ident.Identity
	Secret    string
	RelayPort int

	mu        sync.Mutex
	exporters map[string]*exporter

	relay *relay
}

type exporter struct {
	name        string
	fp          string
	candidates  []string
	defaultPort int
	stream      *quic.Stream
	writeMu     sync.Mutex
}

// Run serves the control listener and the relay socket until ctx ends.
func (s *Server) Run(ctx context.Context, ctrlAddr, relayAddr string) error {
	s.exporters = map[string]*exporter{}

	ru, err := net.ResolveUDPAddr("udp", relayAddr)
	if err != nil {
		return err
	}
	rc, err := net.ListenUDP("udp", ru)
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
	cc, err := net.ListenUDP("udp", cu)
	if err != nil {
		return fmt.Errorf("control listen: %w", err)
	}
	// A stateless reset key derived from the (possibly persistent) identity
	// lets nodes notice a restarted server on their very next packet.
	srk := quic.StatelessResetKey(sha256.Sum256(append([]byte("msnw-srk:"), s.Ident.Cert.PrivateKey.(ed25519.PrivateKey).Seed()...)))
	tr := &quic.Transport{Conn: cc, StatelessResetKey: &srk}
	defer tr.Close()
	ln, err := tr.Listen(s.Ident.ServerConfig(nil), &quic.Config{
		MaxIdleTimeout:  45 * time.Second,
		KeepAlivePeriod: 15 * time.Second,
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

	if !ident.AuthOK(s.Secret, m.Auth, conn.ConnectionState().TLS) {
		log.Printf("%s: %s %q: bad auth", remote, m.Type, m.Name)
		proto.Write(st, &proto.Message{Type: proto.TypeError, Error: "authentication failed"})
		return
	}
	if m.Name == "" || m.Fingerprint == "" {
		proto.Write(st, &proto.Message{Type: proto.TypeError, Error: "name and fp required"})
		return
	}
	cands := netutil.Dedup(append([]string{remote}, m.LocalAddrs...))

	switch m.Type {
	case proto.TypeRegister:
		s.handleRegister(ctx, st, m, cands, remote)
	case proto.TypeConnect:
		s.handleConnect(st, m, cands, remote)
	default:
		proto.Write(st, &proto.Message{Type: proto.TypeError, Error: "unknown message type"})
	}
}

func (s *Server) handleRegister(ctx context.Context, st *quic.Stream, m *proto.Message, cands []string, remote string) {
	ex := &exporter{
		name:        m.Name,
		fp:          ident.NormalizeFP(m.Fingerprint),
		candidates:  cands,
		defaultPort: m.DefaultPort,
		stream:      st,
	}
	s.mu.Lock()
	old := s.exporters[m.Name]
	s.exporters[m.Name] = ex
	s.mu.Unlock()
	if old != nil {
		log.Printf("%s: exporter %q re-registered (replacing %s)", remote, m.Name, old.candidates[0])
		old.send(&proto.Message{Type: proto.TypeError, Error: "replaced by a new registration"})
		old.stream.CancelRead(0)
	} else {
		log.Printf("%s: exporter %q registered, candidates=%v", remote, m.Name, cands)
	}
	if err := ex.send(&proto.Message{Type: proto.TypeOK, Reflexive: remote}); err != nil {
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
		log.Printf("%s: exporter %q gone", remote, m.Name)
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
		log.Printf("%s: connect %q: no such exporter", remote, m.Name)
		proto.Write(st, &proto.Message{Type: proto.TypeError, Error: "no such exporter: " + m.Name})
		return
	}
	session := newSession()
	s.relay.allow(session)
	log.Printf("%s: connect %q -> %s session=%s", remote, m.Name, ex.candidates[0], session[:8])

	err := ex.send(&proto.Message{
		Type:            proto.TypeIncoming,
		Session:         session,
		PeerFingerprint: ident.NormalizeFP(m.Fingerprint),
		Candidates:      cands,
		RelayPort:       s.RelayPort,
	})
	if err != nil {
		proto.Write(st, &proto.Message{Type: proto.TypeError, Error: "exporter unreachable: " + err.Error()})
		return
	}
	proto.Write(st, &proto.Message{
		Type:            proto.TypePeer,
		Session:         session,
		PeerFingerprint: ex.fp,
		Candidates:      ex.candidates,
		RelayPort:       s.RelayPort,
		DefaultPort:     ex.defaultPort,
	})
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
type relay struct {
	conn *net.UDPConn
	mu   sync.Mutex
	sess map[string]*relaySession
	addr map[string]*relaySession // "ip:port" -> session
}

type relaySession struct {
	id       string
	client   *net.UDPAddr
	exporter *net.UDPAddr
	created  time.Time
	lastSeen time.Time
}

func newRelay(c *net.UDPConn) *relay {
	return &relay{conn: c, sess: map[string]*relaySession{}, addr: map[string]*relaySession{}}
}

func (r *relay) allow(session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sess[session] = &relaySession{id: session, created: time.Now(), lastSeen: time.Now()}
}

func (r *relay) run(ctx context.Context) {
	go r.reap(ctx)
	buf := make([]byte, 65535)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		r.handle(buf[:n], from)
	}
}

func (r *relay) handle(b []byte, from *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, role, ok := proto.ParseRelayHello(b); ok {
		rs := r.sess[session]
		if rs == nil {
			return
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
		return
	}
	rs := r.addr[from.String()]
	if rs == nil {
		return
	}
	var to *net.UDPAddr
	if rs.client != nil && rs.client.String() == from.String() {
		to = rs.exporter
	} else {
		to = rs.client
	}
	if to == nil {
		return
	}
	rs.lastSeen = time.Now()
	r.conn.WriteToUDP(b, to)
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
