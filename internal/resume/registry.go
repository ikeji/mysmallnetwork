package resume

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Registry holds sessions by token so a RESUME arriving on any connection
// can find its session. It closes sessions that stay detached past Grace.
type Registry struct {
	mu sync.Mutex
	m  map[string]*Session
}

func NewRegistry() *Registry { return &Registry{m: map[string]*Session{}} }

func (r *Registry) Add(s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[s.Token] = s
}

func (r *Registry) Get(token string) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[token]
}

func (r *Registry) Remove(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, token)
}

// Reap closes sessions detached for longer than grace and forgets closed ones.
func (r *Registry) Reap(grace time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for tok, s := range r.m {
		if detached, since := s.Detached(); detached && time.Since(since) > grace {
			s.Close()
		}
		if s.Closed() {
			delete(r.m, tok)
		}
	}
}

// Run reaps periodically until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Reap(Grace)
		}
	}
}

// NewToken returns a random 128-bit session token.
func NewToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
