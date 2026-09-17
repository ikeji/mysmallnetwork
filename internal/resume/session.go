package resume

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	ackEvery    = 64 * 1024       // send an ACK after this many new bytes
	ackInterval = time.Second     // and at least this often while data flows
	maxUnacked  = 4 * 1024 * 1024 // Write blocks beyond this much unacknowledged data

	// Grace is how long a detached session waits for a resume before giving up.
	Grace = 5 * time.Minute
)

var (
	ErrClosed   = errors.New("resume: session closed")
	ErrBadState = errors.New("resume: peer state does not match")
)

// Session is one resumable connection. It implements net.Conn so callers can
// pipe it to a local socket exactly like a plain stream. Read returns what
// the peer sent; Write is buffered until the peer acknowledges it.
type Session struct {
	Token string
	// OnDetach, if set, is called (in its own goroutine) whenever the stream
	// carrying the session is lost. The client uses it to start a resume.
	OnDetach func(*Session)

	mu   sync.Mutex
	cond *sync.Cond
	st   *quic.Stream // attached stream, nil while detached
	wmu  sync.Mutex   // serializes frame writes; held for the whole replay on attach

	sent, acked uint64 // payload bytes sent / acknowledged by the peer
	recv        uint64 // payload bytes received from the peer
	lastAck     uint64 // recv count last announced to the peer
	replay      []byte // bytes [acked, sent)

	localFin, peerFin bool
	closed            bool
	detachedAt        time.Time

	pr *io.PipeReader
	pw *io.PipeWriter
}

// New creates a detached session.
func New(token string) *Session {
	s := &Session{Token: token, detachedAt: time.Now()}
	s.cond = sync.NewCond(&s.mu)
	s.pr, s.pw = io.Pipe()
	return s
}

// Recv is the number of payload bytes received so far (sent in RESUME).
func (s *Session) Recv() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recv
}

// CanResume reports whether a peer that has received peerRecv bytes can be
// attached: the count must lie within what we still hold.
func (s *Session) CanResume(peerRecv uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && peerRecv >= s.acked && peerRecv <= s.sent
}

// Detached returns whether the session is waiting for a resume and since when.
func (s *Session) Detached() (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st == nil && !s.closed, s.detachedAt
}

// Closed reports whether the session has ended.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Attach binds the session to a fresh stream whose peer has received
// peerRecv bytes, retransmitting whatever it has not seen.
func (s *Session) Attach(st *quic.Stream, rd *bufio.Reader, peerRecv uint64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if peerRecv < s.acked || peerRecv > s.sent {
		s.mu.Unlock()
		return ErrBadState
	}
	if old := s.st; old != nil {
		old.CancelRead(0)
		old.CancelWrite(0)
	}
	s.trimLocked(peerRecv)
	s.st = st
	s.detachedAt = time.Time{}
	pending := append([]byte(nil), s.replay...)
	fin := s.localFin
	s.cond.Broadcast()
	s.mu.Unlock()

	go s.readLoop(st, rd)
	go s.ackLoop(st)

	for len(pending) > 0 {
		n := min(len(pending), maxPayload)
		if err := writeFrame(st, frameData, pending[:n]); err != nil {
			s.detach(st)
			return err
		}
		pending = pending[n:]
	}
	if fin {
		if err := writeFrame(st, frameFin, nil); err != nil {
			s.detach(st)
			return err
		}
	}
	return nil
}

// trimLocked drops replay bytes the peer has acknowledged. Caller holds mu.
func (s *Session) trimLocked(acked uint64) {
	if acked <= s.acked {
		return
	}
	s.replay = s.replay[acked-s.acked:]
	s.acked = acked
}

// current returns the attached stream, or nil.
func (s *Session) current() *quic.Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// detach drops st if it is still the attached stream and notifies OnDetach.
func (s *Session) detach(st *quic.Stream) {
	s.mu.Lock()
	if s.st != st {
		s.mu.Unlock()
		return
	}
	s.st = nil
	s.detachedAt = time.Now()
	closed := s.closed
	s.mu.Unlock()
	st.CancelRead(0)
	st.CancelWrite(0)
	if !closed && s.OnDetach != nil {
		go s.OnDetach(s)
	}
}

// writeOn writes a frame to st if st is still the attached stream. Data that
// is skipped here is already covered by the replay of a newer attach.
func (s *Session) writeOn(st *quic.Stream, typ byte, payload []byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.current() != st {
		return
	}
	if err := writeFrame(st, typ, payload); err != nil {
		s.detach(st)
	}
}

func (s *Session) readLoop(st *quic.Stream, rd *bufio.Reader) {
	for {
		typ, payload, err := readFrame(rd)
		if err != nil {
			s.detach(st)
			return
		}
		switch typ {
		case frameData:
			if _, err := s.pw.Write(payload); err != nil {
				s.Close() // local reader is gone
				return
			}
			s.mu.Lock()
			s.recv += uint64(len(payload))
			due := s.recv-s.lastAck >= ackEvery
			s.mu.Unlock()
			if due {
				s.sendAck(st)
			}
		case frameAck:
			if len(payload) == 8 {
				v := uint64(payload[0])<<56 | uint64(payload[1])<<48 | uint64(payload[2])<<40 | uint64(payload[3])<<32 |
					uint64(payload[4])<<24 | uint64(payload[5])<<16 | uint64(payload[6])<<8 | uint64(payload[7])
				s.mu.Lock()
				if v <= s.sent {
					s.trimLocked(v)
					s.cond.Broadcast()
				}
				s.mu.Unlock()
			}
		case frameFin:
			s.mu.Lock()
			s.peerFin = true
			s.mu.Unlock()
			s.pw.Close()
			s.maybeFinish()
		case frameAbort:
			s.Close()
			return
		}
	}
}

func (s *Session) ackLoop(st *quic.Stream) {
	t := time.NewTicker(ackInterval)
	defer t.Stop()
	for range t.C {
		if s.current() != st {
			return
		}
		s.mu.Lock()
		due := s.recv > s.lastAck
		s.mu.Unlock()
		if due {
			s.sendAck(st)
		}
	}
}

func (s *Session) sendAck(st *quic.Stream) {
	s.mu.Lock()
	n := s.recv
	s.lastAck = n
	s.mu.Unlock()
	s.writeOn(st, frameAck, ackPayload(n))
}

// Read returns data received from the peer.
func (s *Session) Read(b []byte) (int, error) { return s.pr.Read(b) }

// Write buffers b for the peer and sends it if a stream is attached. It
// blocks while too much data is unacknowledged.
func (s *Session) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		n := min(len(b), maxPayload)
		chunk := b[:n]
		s.mu.Lock()
		for !s.closed && s.sent-s.acked > maxUnacked {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return total, ErrClosed
		}
		s.replay = append(s.replay, chunk...)
		s.sent += uint64(n)
		st := s.st
		s.mu.Unlock()
		if st != nil {
			s.writeOn(st, frameData, chunk)
		}
		b = b[n:]
		total += n
	}
	return total, nil
}

// CloseWrite tells the peer that no more data will follow.
func (s *Session) CloseWrite() error {
	s.mu.Lock()
	s.localFin = true
	st := s.st
	s.mu.Unlock()
	if st != nil {
		s.writeOn(st, frameFin, nil)
	}
	s.maybeFinish()
	return nil
}

// maybeFinish ends the session once both directions are done.
func (s *Session) maybeFinish() {
	s.mu.Lock()
	if s.closed || !s.localFin || !s.peerFin {
		s.mu.Unlock()
		return
	}
	s.closed = true
	st := s.st
	s.st = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	if st != nil {
		st.Close()
	}
	s.pw.Close()
}

// Close aborts the session on both sides.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	st := s.st
	s.st = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	if st != nil {
		s.wmu.Lock()
		writeFrame(st, frameAbort, nil)
		s.wmu.Unlock()
		st.Close()
		st.CancelRead(0)
	}
	s.pw.CloseWithError(ErrClosed)
	s.pr.Close()
	return nil
}

// net.Conn plumbing; deadlines are not supported.
func (s *Session) LocalAddr() net.Addr                { return sessionAddr(s.Token) }
func (s *Session) RemoteAddr() net.Addr               { return sessionAddr(s.Token) }
func (s *Session) SetDeadline(t time.Time) error      { return nil }
func (s *Session) SetReadDeadline(t time.Time) error  { return nil }
func (s *Session) SetWriteDeadline(t time.Time) error { return nil }

type sessionAddr string

func (a sessionAddr) Network() string { return "msnw" }
func (a sessionAddr) String() string  { return "session:" + string(a)[:8] }
