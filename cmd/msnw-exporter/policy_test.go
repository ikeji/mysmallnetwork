package main

import "testing"

func TestPolicyResolve(t *testing.T) {
	p := &policy{targets: []string{"localhost:1234", "10.0.0.5:80"}}
	cases := []struct {
		req, want string
		all, ok   bool
	}{
		{"", "localhost:1234", false, true},
		{"1234", "localhost:1234", false, true},
		{"80", "10.0.0.5:80", false, true},
		{"10.0.0.5:80", "10.0.0.5:80", false, true},
		{"9", "", false, false},
		{"example.com:443", "", false, false},
		{"9", "localhost:9", true, true},
		{"example.com:443", "example.com:443", true, true},
	}
	for _, c := range cases {
		p.all = c.all
		got, err := p.resolve(c.req)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("resolve(%q, all=%v) = %q, %v; want %q ok=%v", c.req, c.all, got, err, c.want, c.ok)
		}
	}
	if _, err := (&policy{all: true}).resolve(""); err == nil {
		t.Error("expected error: no default target")
	}
}
