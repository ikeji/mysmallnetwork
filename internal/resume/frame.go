// Package resume implements TCP sessions that survive the loss of the QUIC
// connection carrying them. Each end keeps its local socket open, counts the
// payload bytes it has sent and received, and keeps unacknowledged bytes in
// a replay buffer. After the client reconnects, both ends exchange their
// received counts and retransmit whatever the other has not seen.
//
// Wire format on the stream, after the request/response line:
//
//	[type:1][length:2 big-endian][payload]
//
//	DATA  payload bytes
//	ACK   8-byte total received count
//	FIN   the sender will write no more data
//	ABORT the sender's side of the session is dead
package resume

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

const (
	frameData  byte = 0
	frameAck   byte = 1
	frameFin   byte = 2
	frameAbort byte = 3

	maxPayload = 16 * 1024
)

var errBadFrame = errors.New("resume: bad frame")

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxPayload {
		return errBadFrame
	}
	buf := make([]byte, 3+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint16(buf[1:], uint16(len(payload)))
	copy(buf[3:], payload)
	_, err := w.Write(buf)
	return err
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	if n > maxPayload {
		return 0, nil, errBadFrame
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func ackPayload(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}
