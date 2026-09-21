package cli

import "testing"

func TestPolicyResolve(t *testing.T) {
	mk := func(specs ...string) []target {
		var out []target
		for _, sp := range specs {
			tg, err := parseTarget(sp)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, tg)
		}
		return out
	}
	p := &policy{tcp: mk("1234", "10.0.0.5:80"), udp: mk("60001-60010")}
	cases := []struct {
		proto, req, want string
		all, ok          bool
	}{
		{"tcp", "", "localhost:1234", false, true},
		{"tcp", "1234", "localhost:1234", false, true},
		{"tcp", "80", "10.0.0.5:80", false, true},
		{"tcp", "10.0.0.5:80", "10.0.0.5:80", false, true},
		{"tcp", "9", "", false, false},
		{"tcp", "example.com:443", "", false, false},
		{"tcp", "9", "localhost:9", true, true},
		{"tcp", "example.com:443", "example.com:443", true, true},
		{"udp", "", "localhost:60001", false, true},
		{"udp", "60001", "localhost:60001", false, true},
		{"udp", "60010", "localhost:60010", false, true}, // inside the range
		{"udp", "60011", "", false, false},               // just outside
		{"udp", "1234", "", false, false},                // a TCP-only port is not a UDP target
		{"tcp", "60001", "", false, false},               // and vice versa
		{"udp", "5353", "localhost:5353", true, true},
	}
	for _, c := range cases {
		p.all = c.all
		got, err := p.resolve(c.proto, c.req)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("resolve(%s, %q, all=%v) = %q, %v; want %q ok=%v", c.proto, c.req, c.all, got, err, c.want, c.ok)
		}
	}
	// "~port" hints from SOCKS5: a single-target exporter is a service and
	// ignores the port; several targets or --all make it a host, where the
	// port must match.
	svc := &policy{tcp: mk("8765")}
	for _, req := range []string{"~80", "~443", "~8765"} {
		if got, err := svc.resolve("tcp", req); err != nil || got != "localhost:8765" {
			t.Errorf("service resolve(%q) = %q, %v", req, got, err)
		}
	}
	if _, err := svc.resolve("tcp", "80"); err == nil {
		t.Error("an explicit wrong port must still fail on a service")
	}
	p.all = false
	if got, err := p.resolve("tcp", "~80"); err != nil || got != "10.0.0.5:80" {
		t.Errorf("host resolve(~80) = %q, %v", got, err)
	}
	if _, err := p.resolve("tcp", "~81"); err == nil {
		t.Error("a host with several targets must reject an unexported hinted port")
	}
	if got, err := (&policy{tcp: mk("8765"), all: true}).resolve("tcp", "~80"); err != nil || got != "localhost:80" {
		t.Errorf("--all resolve(~80) = %q, %v", got, err)
	}
	if _, err := (&policy{all: true}).resolve("tcp", ""); err == nil {
		t.Error("expected error: no default target")
	}
	for _, bad := range []string{"", "x", "70000", "10-5", ":22", "0"} {
		if _, err := parseTarget(bad); err == nil {
			t.Errorf("parseTarget(%q) should fail", bad)
		}
	}
}
