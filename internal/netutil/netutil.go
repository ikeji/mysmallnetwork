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

// OptionalValueFlag rewrites os.Args so that a flag which may appear with or
// without a value ("-l", "-l 1234", "-l=1234") can be parsed by the standard
// flag package. A bare occurrence becomes "-<name>=<bare>".
func OptionalValueFlag(args []string, name, bare string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if a == "-"+name || a == "--"+name {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				out = append(out, "-"+name+"="+args[i+1])
				i++
			} else {
				out = append(out, "-"+name+"="+bare)
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// SplitName parses "name", "name:port" or "name:host:port" into the exporter
// name and the target request string ("" = exporter default).
func SplitName(s string) (name, target string) {
	i := strings.Index(s, ":")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}
