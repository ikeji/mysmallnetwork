package cli

import (
	"net"
	"strings"
	"testing"

	"github.com/ikeji/mysmallnetwork/internal/netutil"
)

func TestBindForward(t *testing.T) {
	free := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return ln.Addr().(*net.TCPAddr).Port
	}
	p1, p2 := free(), free()
	cases := []struct {
		fwd         netutil.Forward
		defaultPort int
		wantPort    int
		wantErr     string
	}{
		{netutil.Forward{Host: "127.0.0.1", Name: "n"}, p1, p1, ""},                          // NAME: exporter default
		{netutil.Forward{Host: "127.0.0.1", Port: p2, Name: "n", Target: "443"}, p1, p2, ""}, // explicit local port
		{netutil.Forward{Host: "127.0.0.1", Port: p2, Name: "n", UDP: true}, p1, p2, ""},
		{netutil.Forward{Host: "127.0.0.1", Name: "n"}, 0, 0, "no default port"},
	}
	for _, c := range cases {
		ln, err := bindForward(c.fwd, c.defaultPort)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("bindForward(%+v) err = %v, want %q", c.fwd, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("bindForward(%+v): %v", c.fwd, err)
			continue
		}
		var got int
		if c.fwd.UDP {
			got = ln.udp.LocalAddr().(*net.UDPAddr).Port
			ln.udp.Close()
		} else {
			got = ln.tcp.Addr().(*net.TCPAddr).Port
			ln.tcp.Close()
		}
		if got != c.wantPort || (ln.udp != nil) != c.fwd.UDP {
			t.Errorf("bindForward(%+v) bound %d (udp %v), want %d", c.fwd, got, ln.udp != nil, c.wantPort)
		}
	}
}
