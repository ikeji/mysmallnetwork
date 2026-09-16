// Package socks5 is a minimal SOCKS5 (RFC 1928) server supporting only the
// CONNECT command with no authentication. Connection establishment is
// delegated to a Dialer callback so the caller decides how to reach hosts.
package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// Dialer opens a connection to host:port on behalf of a SOCKS client.
type Dialer func(ctx context.Context, host string, port int) (net.Conn, error)

// Serve accepts SOCKS5 clients on ln until it is closed.
func Serve(ctx context.Context, ln net.Listener, dial Dialer, logf func(string, ...any)) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			if err := handle(ctx, c, dial); err != nil && logf != nil {
				logf("socks5 %s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

func handle(ctx context.Context, c net.Conn, dial Dialer) error {
	defer c.Close()
	// greeting
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != 5 {
		return fmt.Errorf("not socks5 (ver %d)", hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil { // no auth
		return err
	}
	// request
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return err
	}
	if req[1] != 1 { // CONNECT
		reply(c, 7)
		return errors.New("unsupported command")
	}
	var host string
	switch req[3] {
	case 1:
		var b [4]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return err
		}
		host = net.IP(b[:]).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return err
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return err
		}
		host = string(b)
	case 4:
		var b [16]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return err
		}
		host = net.IP(b[:]).String()
	default:
		reply(c, 8)
		return errors.New("unsupported address type")
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return err
	}
	port := int(binary.BigEndian.Uint16(pb[:]))

	rc, err := dial(ctx, host, port)
	if err != nil {
		reply(c, 1)
		return fmt.Errorf("connect %s: %w", net.JoinHostPort(host, strconv.Itoa(port)), err)
	}
	defer rc.Close()
	if err := reply(c, 0); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(rc, c); closeWrite(rc); done <- struct{}{} }()
	go func() { io.Copy(c, rc); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}

func reply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
