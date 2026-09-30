// Package webterm serves a browser terminal (xterm.js) whose sessions are
// SSH connections made through the msnw tunnel. It exists so that a thin
// shell such as the Android app can offer a terminal by pointing a WebView at
// http://127.0.0.1:<port>/ while all the logic stays in Go. Because the SSH
// connection rides on a resumable msnw session, it survives network changes
// the way mosh does (without mosh's local echo).
//
// Protocol on /ws: the first text frame from the browser is a JSON
// {"target":"user@name[:port]","password":"...","cols":N,"rows":N}; later
// text frames are {"resize":[cols,rows]}; binary frames are terminal input.
// Frames from the server are terminal output (binary) or a text status line
// (prefixed with "\x00" so the page can tell them apart).
package webterm

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

//go:embed static index.html
var content embed.FS

// Dialer opens a tunnelled connection to port on exporter name.
type Dialer func(ctx context.Context, name, port string) (net.Conn, error)

// Server is the web terminal.
type Server struct {
	Dial       Dialer
	KnownHosts string // path of the known_hosts file (created on first use)
	KeyFile    string // optional private key for public-key authentication
	Logf       func(string, ...any)
}

// Handler returns the HTTP handler: the page, its assets and /ws.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(content, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := content.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/ws", s.serveWS)
	return mux
}

type hello struct {
	Target   string `json:"target"`
	Password string `json:"password"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
}

type control struct {
	Resize []int `json:"resize"`
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"127.0.0.1:*", "localhost:*"}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx := r.Context()
	status := func(format string, args ...any) {
		ws.Write(ctx, websocket.MessageText, []byte("\x00"+fmt.Sprintf(format, args...)))
	}

	// 1. hello
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	typ, msg, err := ws.Read(hctx)
	cancel()
	if err != nil || typ != websocket.MessageText {
		return
	}
	var h hello
	if err := json.Unmarshal(msg, &h); err != nil || h.Target == "" {
		status("bad hello")
		return
	}
	user, name, port := splitTarget(h.Target)
	if user == "" || name == "" {
		status("target must be user@name[:port]")
		return
	}

	// 2. ssh through the tunnel
	status("connecting to %s ...", name)
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	raw, err := s.Dial(dctx, name, port)
	cancel()
	if err != nil {
		status("connect failed: %v", err)
		return
	}
	defer raw.Close()
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            s.authMethods(h.Password),
		HostKeyCallback: s.hostKeyCallback(name, status),
		Timeout:         60 * time.Second,
	}
	shown := port
	if shown == "" {
		shown = "22" // the exporter's default target; recorded under this port
	}
	cc, chans, reqs, err := ssh.NewClientConn(raw, name+":"+shown, cfg)
	if err != nil {
		status("ssh: %v", err)
		return
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		status("ssh session: %v", err)
		return
	}
	defer sess.Close()
	cols, rows := h.Cols, h.Rows
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		status("pty: %v", err)
		return
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return
	}
	sess.Stderr = &wsWriter{ws: ws, ctx: ctx}
	if err := sess.Shell(); err != nil {
		status("shell: %v", err)
		return
	}
	status("connected")
	s.logf("webterm: %s@%s:%s session opened", user, name, shown)

	// 3. pump
	var once sync.Once
	done := make(chan struct{})
	finish := func() { once.Do(func() { close(done) }) }
	go func() { // ssh -> browser (the shell's exit, below, ends the session)
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { // browser -> ssh
		defer finish()
		for {
			typ, msg, err := ws.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := stdin.Write(msg); err != nil {
					return
				}
			case websocket.MessageText:
				var c control
				if json.Unmarshal(msg, &c) == nil && len(c.Resize) == 2 {
					sess.WindowChange(c.Resize[1], c.Resize[0])
				}
			}
		}
	}()
	go func() { // shell exit
		defer finish()
		err := sess.Wait()
		var ee *ssh.ExitError
		switch {
		case err == nil:
			status("session closed")
		case errors.As(err, &ee):
			status("session closed (exit %d)", ee.ExitStatus())
		default:
			status("session closed: %v", err)
		}
	}()
	<-done
	s.logf("webterm: %s@%s:%s session closed", user, name, shown)
	ws.Close(websocket.StatusNormalClosure, "")
}

type wsWriter struct {
	ws  *websocket.Conn
	ctx context.Context
}

func (w *wsWriter) Write(p []byte) (int, error) {
	return len(p), w.ws.Write(w.ctx, websocket.MessageBinary, p)
}

func (s *Server) authMethods(password string) []ssh.AuthMethod {
	var m []ssh.AuthMethod
	if s.KeyFile != "" {
		if b, err := os.ReadFile(s.KeyFile); err == nil {
			if signer, err := ssh.ParsePrivateKey(b); err == nil {
				m = append(m, ssh.PublicKeys(signer))
			} else {
				s.logf("webterm: key %s: %v", s.KeyFile, err)
			}
		}
	}
	if password != "" {
		m = append(m, ssh.Password(password))
		m = append(m, ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			ans := make([]string, len(questions))
			for i := range ans {
				ans[i] = password
			}
			return ans, nil
		}))
	}
	return m
}

// hostKeyCallback trusts a host on first use and records it in KnownHosts,
// then insists on the same key afterwards. The record is keyed by the
// exporter name, which is what identifies the host in msnw.
func (s *Server) hostKeyCallback(name string, status func(string, ...any)) ssh.HostKeyCallback {
	return func(hostport string, _ net.Addr, key ssh.PublicKey) error {
		if s.KnownHosts == "" {
			return nil
		}
		// The library wants host:port and a parseable remote address; the
		// tunnel's addresses are not real, so give it a placeholder.
		remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}
		if _, err := os.Stat(s.KnownHosts); errors.Is(err, os.ErrNotExist) {
			os.MkdirAll(filepath.Dir(s.KnownHosts), 0o700)
			if f, err := os.OpenFile(s.KnownHosts, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.Close()
			}
		}
		check, err := knownhosts.New(s.KnownHosts)
		if err != nil {
			return err
		}
		err = check(hostport, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 { // unknown host: trust on first use
			line := knownhosts.Line([]string{knownhosts.Normalize(hostport)}, key)
			f, err := os.OpenFile(s.KnownHosts, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			fmt.Fprintln(f, line)
			f.Close()
			status("new host %s, key %s recorded", name, ssh.FingerprintSHA256(key))
			return nil
		}
		return fmt.Errorf("host key for %s changed (%s); remove it from %s if this is expected", name, ssh.FingerprintSHA256(key), s.KnownHosts)
	}
}

// splitTarget parses "user@name[:port]". An empty port means the exporter's
// default target (normally its sshd).
func splitTarget(t string) (user, name, port string) {
	if i := strings.LastIndex(t, "@"); i >= 0 {
		user, t = t[:i], t[i+1:]
	}
	if h, p, err := net.SplitHostPort(t); err == nil {
		if _, err := strconv.Atoi(p); err == nil {
			t, port = h, p
		}
	}
	return user, t, port
}

// Serve runs the web terminal on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler()}
	go func() { <-ctx.Done(); srv.Close() }()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}
