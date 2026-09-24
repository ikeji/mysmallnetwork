// Package httpproxy is a minimal HTTP proxy: CONNECT for tunnelled (TLS)
// connections and absolute-URI forwarding for plain HTTP. Connection
// establishment is delegated to a Dialer so the caller decides how to reach
// hosts, exactly as in package socks5. Browsers that cannot use SOCKS (Android
// WebView, the Android Wi-Fi proxy setting) can use this instead.
package httpproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Dialer opens a connection to host:port on behalf of a proxy client.
type Dialer func(ctx context.Context, host string, port int) (net.Conn, error)

// Serve accepts proxy clients on ln until it is closed.
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
				logf("http proxy %s: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

func handle(ctx context.Context, c net.Conn, dial Dialer) error {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Minute))
		req, err := http.ReadRequest(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		c.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			return connect(ctx, c, br, req, dial)
		}
		keep, err := forward(ctx, c, req, dial)
		if err != nil || !keep {
			return err
		}
	}
}

// connect handles "CONNECT host:port": answer 200 and pipe bytes both ways.
func connect(ctx context.Context, c net.Conn, br *bufio.Reader, req *http.Request, dial Dialer) error {
	host, port, err := splitHostPort(req.Host, 443)
	if err != nil {
		reply(c, http.StatusBadRequest, err.Error())
		return err
	}
	rc, err := dial(ctx, host, port)
	if err != nil {
		reply(c, http.StatusBadGateway, err.Error())
		return fmt.Errorf("connect %s: %w", req.Host, err)
	}
	defer rc.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return err
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(rc, br); closeWrite(rc); done <- struct{}{} }()
	go func() { io.Copy(c, rc); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

// forward relays one plain HTTP request given with an absolute URI and
// streams the response back. It reports whether the client connection may
// carry another request.
func forward(ctx context.Context, c net.Conn, req *http.Request, dial Dialer) (bool, error) {
	if !req.URL.IsAbs() {
		reply(c, http.StatusBadRequest, "this is a proxy; request an absolute URL")
		return false, fmt.Errorf("non-proxy request %s %s", req.Method, req.URL)
	}
	if req.URL.Scheme != "http" {
		reply(c, http.StatusBadRequest, "only http:// can be forwarded; use CONNECT for "+req.URL.Scheme)
		return false, fmt.Errorf("unsupported scheme %s", req.URL.Scheme)
	}
	host, port, err := splitHostPort(req.URL.Host, 80)
	if err != nil {
		reply(c, http.StatusBadRequest, err.Error())
		return false, err
	}
	rc, err := dial(ctx, host, port)
	if err != nil {
		reply(c, http.StatusBadGateway, err.Error())
		return false, fmt.Errorf("forward %s: %w", req.URL.Host, err)
	}
	defer rc.Close()

	// Turn the proxy request into an origin request: relative URI, no
	// hop-by-hop headers, one request per upstream connection.
	req.RequestURI = ""
	req.Host = req.URL.Host
	for _, h := range hopByHop(req.Header) {
		req.Header.Del(h)
	}
	req.Header.Set("Connection", "close")
	if err := req.Write(rc); err != nil {
		return false, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(rc), req)
	if err != nil {
		reply(c, http.StatusBadGateway, err.Error())
		return false, err
	}
	defer resp.Body.Close()
	for _, h := range hopByHop(resp.Header) {
		resp.Header.Del(h)
	}
	keep := !req.Close && req.ProtoAtLeast(1, 1) && strings.ToLower(req.Header.Get("Proxy-Connection")) != "close"
	if keep {
		resp.Header.Set("Connection", "keep-alive")
	} else {
		resp.Header.Set("Connection", "close")
	}
	if err := resp.Write(c); err != nil {
		return false, err
	}
	return keep, nil
}

func hopByHop(h http.Header) []string {
	out := []string{"Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade", "Connection"}
	for _, v := range h.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

func splitHostPort(hp string, def int) (string, int, error) {
	host, ps, err := net.SplitHostPort(hp)
	if err != nil {
		return strings.Trim(hp, "[]"), def, nil
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return "", 0, fmt.Errorf("bad port in %q", hp)
	}
	return host, port, nil
}

func reply(c net.Conn, code int, msg string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s\n",
		code, http.StatusText(code), len(msg)+1, msg)
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}
