// Package proto defines the control protocol spoken between the rendezvous
// server and the exporter/client nodes, plus the raw UDP packet formats used
// for NAT hole punching and relay signalling.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ALPN is the TLS application protocol for both control and peer connections.
const ALPN = "msnw/1"

// Message types.
const (
	TypeRegister = "register" // exporter -> server
	TypeConnect  = "connect"  // client -> server
	TypeOK       = "ok"       // server -> node
	TypeError    = "error"    // server -> node
	TypeIncoming = "incoming" // server -> exporter (a client wants to connect)
	TypePeer     = "peer"     // server -> client (how to reach the exporter)
)

// Message is the single flat envelope used on control streams. Only the
// fields relevant to a given Type are populated.
type Message struct {
	Type string `json:"type"`

	// Register / Connect
	Name        string   `json:"name,omitempty"`
	Auth        string   `json:"auth,omitempty"` // hex(HMAC-SHA256(secret, tls-exporter))
	Fingerprint string   `json:"fp,omitempty"`   // sha256 of the node's SPKI, hex
	LocalAddrs  []string `json:"local_addrs,omitempty"`
	DefaultPort int      `json:"default_port,omitempty"` // exporter's default target port

	// OK
	Reflexive string `json:"reflexive,omitempty"` // node's public addr as seen by the server

	// Error
	Error string `json:"error,omitempty"`

	// Incoming / Peer
	Session         string   `json:"session,omitempty"`
	PeerFingerprint string   `json:"peer_fp,omitempty"`
	Candidates      []string `json:"candidates,omitempty"`
	RelayPort       int      `json:"relay_port,omitempty"` // relay UDP port on the server host
}

const maxFrame = 64 * 1024

// Write sends one length-prefixed JSON frame.
func Write(w io.Writer, m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return errors.New("proto: frame too large")
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// Read receives one length-prefixed JSON frame.
func Read(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, fmt.Errorf("proto: frame too large (%d)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ---- raw UDP packets -------------------------------------------------------
//
// All raw packets start with a 0x00 byte so that quic-go classifies them as
// non-QUIC (first two bits clear) and never confuses them with QUIC traffic
// on the shared socket.

const (
	punchMagic = "\x00MSNW-PUNCH"
	relayMagic = "\x00MSNW-RELAY"

	RoleClient   = 'c'
	RoleExporter = 'e'
)

// PunchPacket is the payload sent to open a NAT mapping toward a peer.
func PunchPacket(session string) []byte {
	return append([]byte(punchMagic), session...)
}

// ParsePunch returns the session id carried by a punch packet.
func ParsePunch(b []byte) (string, bool) {
	if len(b) < len(punchMagic) || string(b[:len(punchMagic)]) != punchMagic {
		return "", false
	}
	return string(b[len(punchMagic):]), true
}

// RelayHello builds the packet a node sends to the relay port to bind its
// address to a session.
func RelayHello(session string, role byte) []byte {
	b := append([]byte(relayMagic), role)
	return append(b, session...)
}

// ParseRelayHello returns (session, role, true) if b is a relay hello.
func ParseRelayHello(b []byte) (string, byte, bool) {
	if len(b) < len(relayMagic)+1 || string(b[:len(relayMagic)]) != relayMagic {
		return "", 0, false
	}
	role := b[len(relayMagic)]
	if role != RoleClient && role != RoleExporter {
		return "", 0, false
	}
	return string(b[len(relayMagic)+1:]), role, true
}
