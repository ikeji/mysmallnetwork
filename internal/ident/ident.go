// Package ident manages the per-process TLS identity (an ed25519 key with a
// self-signed certificate) and the fingerprint-pinning helpers built on it.
package ident

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"mysmallnetwork/internal/proto"
)

// Identity is a key pair plus its self-signed certificate.
type Identity struct {
	Cert        tls.Certificate
	Fingerprint string // hex sha256 of SubjectPublicKeyInfo
}

// New generates an ephemeral identity.
func New() (*Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return fromKey(priv)
}

// Load reads an ed25519 private key (PKCS#8 PEM) from path, generating and
// saving one if the file does not exist. Fingerprints are computed over the
// public key so they stay stable across restarts.
func Load(path string) (*Identity, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("ident: %s: not PEM", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		priv, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("ident: %s: not an ed25519 key", path)
		}
		return fromKey(priv)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemb := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemb, 0o600); err != nil {
		return nil, err
	}
	return fromKey(priv)
}

func fromKey(priv ed25519.PrivateKey) (*Identity, error) {
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{
		Cert:        tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf},
		Fingerprint: FingerprintOf(leaf),
	}, nil
}

// FingerprintOf returns the hex SHA-256 of the certificate's public key.
func FingerprintOf(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// NormalizeFP accepts "sha256:xx..", "xx:yy:..", upper/lower case.
func NormalizeFP(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "sha256:")
	return strings.ReplaceAll(s, ":", "")
}

func verifyPinned(allowed func(fp string) bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("ident: peer presented no certificate")
		}
		c, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		fp := FingerprintOf(c)
		if !allowed(fp) {
			return fmt.Errorf("ident: peer fingerprint %s not accepted", fp[:16])
		}
		return nil
	}
}

// ClientConfig returns a TLS config for dialing a peer. If expectedFP is
// empty the peer certificate is not verified (only for the rendezvous server
// when no pin is configured; the shared secret still authenticates us and the
// server's answer is trusted at the user's discretion).
func (id *Identity) ClientConfig(expectedFP string) *tls.Config {
	conf := &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // we pin instead of using the CA system
		NextProtos:         []string{proto.ALPN},
		MinVersion:         tls.VersionTLS13,
	}
	if expectedFP != "" {
		want := NormalizeFP(expectedFP)
		conf.VerifyPeerCertificate = verifyPinned(func(fp string) bool { return fp == want })
	}
	return conf
}

// ServerConfig returns a TLS config for accepting connections. If allow is
// non-nil, clients must present a certificate whose fingerprint allow()
// accepts; otherwise client certificates are not requested.
func (id *Identity) ServerConfig(allow func(fp string) bool) *tls.Config {
	conf := &tls.Config{
		Certificates: []tls.Certificate{id.Cert},
		NextProtos:   []string{proto.ALPN},
		MinVersion:   tls.VersionTLS13,
	}
	if allow != nil {
		conf.ClientAuth = tls.RequireAnyClientCert
		conf.VerifyPeerCertificate = verifyPinned(allow)
	}
	return conf
}

// AuthTag computes the shared-secret proof bound to a TLS session, so that it
// cannot be replayed on another connection.
func AuthTag(secret string, cs tls.ConnectionState) (string, error) {
	ekm, err := cs.ExportKeyingMaterial("msnw-auth", nil, 32)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(ekm)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// AuthOK verifies a tag produced by AuthTag.
func AuthOK(secret, tag string, cs tls.ConnectionState) bool {
	want, err := AuthTag(secret, cs)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(want), []byte(tag))
}

// AllowList is a concurrency-safe set of fingerprints with expiry, used by
// exporters to admit exactly the clients the server introduced.
type AllowList struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

func NewAllowList(ttl time.Duration) *AllowList {
	return &AllowList{ttl: ttl, m: map[string]time.Time{}}
}

func (a *AllowList) Add(fp string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[NormalizeFP(fp)] = time.Now().Add(a.ttl)
}

func (a *AllowList) Allowed(fp string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, exp := range a.m {
		if exp.Before(now) {
			delete(a.m, k)
		}
	}
	_, ok := a.m[NormalizeFP(fp)]
	return ok
}
