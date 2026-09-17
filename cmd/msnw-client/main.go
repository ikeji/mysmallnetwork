// msnw-client: reaches services published by msnw-exporter.
//
//	msnw-client -n NAME[:port]            pipe stdin/stdout to the service (nc / ProxyCommand style)
//	msnw-client -n NAME[:port] -l [addr]  listen locally and forward each TCP connection
//	msnw-client --socks5 [addr] [-n NAME] SOCKS5 proxy; hosts "NAME", "NAME.msnw", "host.NAME.msnw"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"mysmallnetwork/internal/ident"
	"mysmallnetwork/internal/netutil"
	"mysmallnetwork/internal/peer"
	"mysmallnetwork/internal/socks5"
	"mysmallnetwork/internal/tunnel"
)

// pool keeps one authenticated peer connection per exporter name and redials
// on failure. The link key is looked up per name so that several keys (with
// priorities) can be supported later without changing callers.
type pool struct {
	node    *peer.Node
	linkKey string
	mu      sync.Mutex
	ents    map[string]*entry
}

type entry struct {
	mu          sync.Mutex
	conn        *quic.Conn
	defaultPort int
}

func (p *pool) keyFor(name string) string { return p.linkKey }

// get returns a live, link-key-authenticated connection to exporter name and
// the exporter's default target port.
func (p *pool) get(ctx context.Context, name string) (*quic.Conn, int, error) {
	p.mu.Lock()
	e := p.ents[name]
	if e == nil {
		e = &entry{}
		p.ents[name] = e
	}
	p.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil && e.conn.Context().Err() == nil {
		return e.conn, e.defaultPort, nil
	}
	key := p.keyFor(name)
	info, err := p.node.Lookup(ctx, key, name)
	if err != nil {
		return nil, 0, err
	}
	conn, via, err := p.node.DialPeer(ctx, info)
	if err != nil {
		return nil, 0, err
	}
	port, err := peer.AuthenticateAsClient(ctx, conn, key)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", name, err)
	}
	log.Printf("connected to %q via %s", name, via)
	e.conn, e.defaultPort = conn, port
	return conn, port, nil
}

// open returns a stream to target on exporter name, already CONNECTed.
func (p *pool) open(ctx context.Context, name, target string) (*quic.Stream, *tunnel.Reader, error) {
	conn, _, err := p.get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, err
	}
	rd, err := tunnel.Open(st, target)
	if err != nil {
		st.CancelRead(0)
		st.Close()
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	return st, rd, nil
}

func main() {
	// quic-go warns loudly when the UDP receive buffer is small; the warning is
	// harmless for a tunnel of this size, and sysctl advice lives in the README.
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	}
	args := netutil.OptionalValueFlag(os.Args[1:], "l", "auto")
	args = netutil.OptionalValueFlag(args, "socks5", "127.0.0.1:1080")
	fs := flag.NewFlagSet("msnw-client", flag.ExitOnError)
	name := fs.String("n", "", "exporter NAME[:port|:host:port] to connect to (default exporter in socks5 mode)")
	listen := fs.String("l", "", "listen locally (port, :port or host:port; bare -l uses the exporter's port)")
	socks := fs.String("socks5", "", "run a SOCKS5 proxy (bare --socks5 listens on 127.0.0.1:1080)")
	server := fs.String("s", envOr("MSNW_SERVER", "localhost:4433"), "rendezvous server host:port (or $MSNW_SERVER)")
	linkKey := fs.String("key", os.Getenv("MSNW_KEY"), "link key shared with the exporter (or $MSNW_KEY); required")
	serverKey := fs.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key (or $MSNW_SERVER_KEY), if the server requires one")
	serverFP := fs.String("server-fp", os.Getenv("MSNW_SERVER_FP"), "pin the server's sha256 fingerprint (or $MSNW_SERVER_FP)")
	port := fs.Int("port", 0, "local UDP port to bind (0 = random)")
	genKey := fs.Bool("gen-key", false, "print a fresh random link key and exit")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Parse(args)

	if *genKey {
		fmt.Println(ident.GenerateKey())
		return
	}
	if *linkKey == "" || (*name == "" && *socks == "") {
		fmt.Fprintln(os.Stderr, "usage: msnw-client -key LINKKEY -n NAME[:port] [-l [addr]] | --socks5 [addr] [-n NAME]   (-s server, -server-key K)")
		os.Exit(2)
	}
	log.SetOutput(os.Stderr)

	id, err := ident.New()
	if err != nil {
		log.Fatal(err)
	}
	node, err := peer.New(id, *server, *serverKey, *serverFP, *port)
	if err != nil {
		log.Fatal(err)
	}
	node.Verbose = *verbose
	defer node.Close()
	p := &pool{node: node, linkKey: *linkKey, ents: map[string]*entry{}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case *socks != "":
		runSocks(ctx, p, *socks, *name)
	case *listen != "":
		runListen(ctx, p, *name, *listen)
	default:
		runStdio(ctx, p, *name)
	}
}

func runStdio(ctx context.Context, p *pool, spec string) {
	name, target := netutil.SplitName(spec)
	st, rd, err := p.open(ctx, name, target)
	if err != nil {
		log.Fatal(err)
	}
	tunnel.PipeRW(st, rd, os.Stdin, os.Stdout)
}

func runListen(ctx context.Context, p *pool, spec, listen string) {
	name, target := netutil.SplitName(spec)
	// Connect eagerly: it validates the name and tells us the default port.
	_, defaultPort, err := p.get(ctx, name)
	if err != nil {
		log.Fatal(err)
	}
	if listen == "auto" {
		port := defaultPort
		if target != "" {
			_, ps, err := net.SplitHostPort(target)
			if err != nil {
				ps = target
			}
			port, _ = strconv.Atoi(ps)
		}
		if port == 0 {
			log.Fatal("-l without a value, but the exporter has no default port; give -l PORT")
		}
		listen = strconv.Itoa(port)
	}
	addr, err := netutil.ParseListen(listen)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	log.Printf("listening on %s -> %s", ln.Addr(), spec)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			st, rd, err := p.open(ctx, name, target)
			if err != nil {
				log.Printf("%s: %v", c.RemoteAddr(), err)
				c.Close()
				return
			}
			tunnel.Pipe(st, rd, c)
		}()
	}
}

// resolveHost maps a SOCKS destination to (exporter, target).
//
//	NAME              -> exporter NAME, port as given
//	NAME.msnw         -> same
//	host.NAME.msnw    -> exporter NAME, target host:port (needs --all or an exact -t)
//	anything else     -> default exporter (-n), target host:port
func resolveHost(host string, port int, def string) (name, target string, err error) {
	ps := strconv.Itoa(port)
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if strings.HasSuffix(h, ".msnw") {
		h = strings.TrimSuffix(h, ".msnw")
		if i := strings.LastIndex(h, "."); i >= 0 {
			return h[i+1:], net.JoinHostPort(h[:i], ps), nil
		}
		return h, ps, nil
	}
	if !strings.Contains(h, ".") && net.ParseIP(h) == nil {
		return h, ps, nil
	}
	if def == "" {
		return "", "", fmt.Errorf("no route for %s (use NAME, NAME.msnw, host.NAME.msnw, or pass -n DEFAULT)", host)
	}
	return def, net.JoinHostPort(host, ps), nil
}

func runSocks(ctx context.Context, p *pool, listen, def string) {
	addr, err := netutil.ParseListen(listen)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	log.Printf("socks5 proxy on %s (default exporter %q)", ln.Addr(), def)
	dial := func(ctx context.Context, host string, port int) (net.Conn, error) {
		name, target, err := resolveHost(host, port, def)
		if err != nil {
			return nil, err
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		st, rd, err := p.open(dctx, name, target)
		if err != nil {
			return nil, err
		}
		return tunnel.NewConn(st, rd), nil
	}
	socks5.Serve(ctx, ln, dial, log.Printf)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
