package cli

import "testing"

func TestPolicyResolve(t *testing.T) {
	p := &policy{tcp: []string{"localhost:1234", "10.0.0.5:80"}, udp: []string{"localhost:60001"}}
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
		{"udp", "1234", "", false, false},  // a TCP-only port is not a UDP target
		{"tcp", "60001", "", false, false}, // and vice versa
		{"udp", "5353", "localhost:5353", true, true},
	}
	for _, c := range cases {
		p.all = c.all
		got, err := p.resolve(c.proto, c.req)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("resolve(%s, %q, all=%v) = %q, %v; want %q ok=%v", c.proto, c.req, c.all, got, err, c.want, c.ok)
		}
	}
	if _, err := (&policy{all: true}).resolve("tcp", ""); err == nil {
		t.Error("expected error: no default target")
	}
}
