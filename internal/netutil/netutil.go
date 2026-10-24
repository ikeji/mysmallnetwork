// Package netutil holds small networking and CLI helpers shared by the commands.
package netutil

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// LocalAddrs lists this host's unicast IP addresses paired with port, for use
// as direct-connection candidates on the same LAN.
func LocalAddrs(port int) []string {
	var out []string
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if !ip.IsGlobalUnicast() && !ip.IsPrivate() {
				continue
			}
			out = append(out, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		}
	}
	return out
}

// Dedup removes duplicates while keeping order.
func Dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ParseListen turns "1234", ":1234", "host:1234" into a listen address.
// A bare port binds to loopback only.
func ParseListen(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty listen address")
	}
	if !strings.Contains(s, ":") {
		if _, err := strconv.Atoi(s); err != nil {
			return "", fmt.Errorf("bad port %q", s)
		}
		return "127.0.0.1:" + s, nil
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return "", err
	}
	return s, nil
}

// Forward is one "msnw import" argument, parsed by ParseForward.
type Forward struct {
	Host   string // local address to bind; "" = all interfaces
	Port   int    // local port; 0 = same as Target's port, or the exporter's default
	UDP    bool
	Name   string // exporter
	Target string // "" (exporter default), "port" or "host:port"
}

// ParseForward parses [[[LADDR]:]LPORT=]NAME[:PORT|@HOST:PORT][/udp].
//
// Left of "=" is the local side as ParseListen reads it: a bare port binds
// 127.0.0.1, ":port" every interface, "addr:port" that address. Right of
// "=", or the whole string without one, is the remote side spelled as for
// "msnw connect". Without a local side the listener is 127.0.0.1 on the
// remote port number.
func ParseForward(s string) (Forward, error) {
	var f Forward
	orig := s
	if rest, ok := strings.CutSuffix(s, "/udp"); ok {
		f.UDP, s = true, rest
	} else if rest, ok := strings.CutSuffix(s, "/tcp"); ok {
		s = rest
	}
	local, remote, hasLocal := strings.Cut(s, "=")
	if !hasLocal {
		local, remote = "", s
	}
	var err error
	if f.Name, f.Target, err = ParseRemote(remote); err != nil {
		return f, fmt.Errorf("%q: %v", orig, err)
	}
	f.Host = "127.0.0.1"
	if hasLocal {
		addr, err := ParseListen(local)
		if err != nil {
			return f, fmt.Errorf("%q: local side: %v (it is port, :port or addr:port)", orig, err)
		}
		h, ps, _ := net.SplitHostPort(addr)
		f.Host = h
		if f.Port, err = port(ps); err != nil {
			return f, fmt.Errorf("%q: local side: %v", orig, err)
		}
	} else if f.Target != "" {
		_, ps, err := net.SplitHostPort(f.Target)
		if err != nil {
			ps = f.Target
		}
		f.Port, _ = port(ps)
	}
	return f, nil
}

// ParseRemote is SplitName with validation: the exporter name must not be
// empty, a ":port" must be a port number and an "@host:port" a host and one.
func ParseRemote(s string) (name, target string, err error) {
	name, target = SplitName(s)
	if name == "" {
		return "", "", fmt.Errorf("missing exporter name (the remote side is NAME, NAME:port or NAME@host:port)")
	}
	if target == "" {
		return name, "", nil
	}
	if strings.Contains(s, "@") {
		h, ps, err := net.SplitHostPort(target)
		if err != nil || h == "" {
			return "", "", fmt.Errorf("%q after @ is not host:port", target)
		}
		if _, err := port(ps); err != nil {
			return "", "", err
		}
		return name, target, nil
	}
	if _, err := port(target); err != nil {
		return "", "", fmt.Errorf("%v (the remote side is NAME, NAME:port or NAME@host:port)", err)
	}
	return name, target, nil
}

func port(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("bad port %q", s)
	}
	return p, nil
}

// Listen returns the address to bind for f once the port is known.
func (f Forward) Listen(port int) string { return net.JoinHostPort(f.Host, strconv.Itoa(port)) }

// Remote is the remote side as "msnw connect" would spell it.
func (f Forward) Remote() string {
	switch {
	case f.Target == "":
		return f.Name
	case strings.Contains(f.Target, ":"):
		return f.Name + "@" + f.Target
	}
	return f.Name + ":" + f.Target
}

// SplitName parses "name", "name:port" or "name@host:port" into the exporter
// name and the target request string ("" = exporter default). Nothing is
// checked; see ParseRemote.
func SplitName(s string) (name, target string) {
	if name, target, ok := strings.Cut(s, "@"); ok {
		return name, target
	}
	name, target, _ = strings.Cut(s, ":")
	return name, target
}
