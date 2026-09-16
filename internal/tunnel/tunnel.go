// Package tunnel defines the per-stream protocol between client and exporter
// and the byte-pumping helpers. Each QUIC stream carries one TCP connection:
//
//	client:   "CONNECT <target>\n"      target = "" | "port" | "host:port"
//	exporter: "OK\n" | "ERR <reason>\n"
//	then raw bytes in both directions until EOF.
package tunnel

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// Open sends a CONNECT for target on st and waits for the exporter's answer.
// The returned reader must be used for subsequent reads (it may hold
// buffered bytes).
func Open(st *quic.Stream, target string) (*bufio.Reader, error) {
	if _, err := fmt.Fprintf(st, "CONNECT %s\n", target); err != nil {
		return nil, err
	}
	br := bufio.NewReader(st)
	st.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadString('\n')
	st.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "OK" {
		return br, nil
	}
	if strings.HasPrefix(line, "ERR ") {
		return nil, errors.New(strings.TrimPrefix(line, "ERR "))
	}
	return nil, fmt.Errorf("bad response %q", line)
}

// Accept reads the CONNECT request from st.
func Accept(st *quic.Stream) (target string, br *bufio.Reader, err error) {
	br = bufio.NewReader(st)
	st.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadString('\n')
	st.SetReadDeadline(time.Time{})
	if err != nil {
		return "", nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "CONNECT ") && line != "CONNECT" {
		return "", nil, fmt.Errorf("bad request %q", line)
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "CONNECT")), br, nil
}

// Reject answers a CONNECT with an error.
func Reject(st *quic.Stream, reason string) {
	fmt.Fprintf(st, "ERR %s\n", strings.ReplaceAll(reason, "\n", " "))
	st.Close()
}

// Confirm answers a CONNECT with OK.
func Confirm(st *quic.Stream) error {
	_, err := io.WriteString(st, "OK\n")
	return err
}

// Pipe copies between a QUIC stream and a TCP-ish connection until both
// directions are finished. rd is the (possibly buffered) reader for st.
func Pipe(st *quic.Stream, rd io.Reader, c net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(st, c) // local -> peer
		st.Close()     // send FIN
	}()
	go func() {
		defer wg.Done()
		io.Copy(c, rd) // peer -> local
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			c.Close()
		}
	}()
	wg.Wait()
	c.Close()
	st.CancelRead(0)
}

// PipeRW is Pipe for an arbitrary reader/writer pair (used for stdio mode).
func PipeRW(st *quic.Stream, rd io.Reader, in io.Reader, out io.Writer) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(st, in)
		st.Close()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(out, rd)
		done <- struct{}{}
	}()
	// Finish when the peer side is done; if stdin closes first we still
	// wait for the remaining output.
	<-done
	<-done
}

// Reader is the buffered reader returned by Open/Accept.
type Reader = bufio.Reader

// Conn adapts a stream plus its buffered reader to net.Conn, with CloseWrite
// mapping to a QUIC FIN.
type Conn struct {
	st *quic.Stream
	rd *bufio.Reader
}

// NewConn wraps st/rd as a net.Conn.
func NewConn(st *quic.Stream, rd *bufio.Reader) *Conn { return &Conn{st: st, rd: rd} }

func (c *Conn) Read(b []byte) (int, error)  { return c.rd.Read(b) }
func (c *Conn) Write(b []byte) (int, error) { return c.st.Write(b) }
func (c *Conn) CloseWrite() error           { return c.st.Close() }
func (c *Conn) Close() error {
	c.st.CancelRead(0)
	return c.st.Close()
}
func (c *Conn) LocalAddr() net.Addr                { return dummyAddr("msnw") }
func (c *Conn) RemoteAddr() net.Addr               { return dummyAddr("msnw") }
func (c *Conn) SetDeadline(t time.Time) error      { return c.st.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.st.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.st.SetWriteDeadline(t) }

type dummyAddr string

func (d dummyAddr) Network() string { return "msnw" }
func (d dummyAddr) String() string  { return string(d) }
