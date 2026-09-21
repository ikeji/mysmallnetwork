package cli

import "testing"

func TestResolveHost(t *testing.T) {
	cases := []struct {
		host              string
		port              int
		def, name, target string
	}{
		{"hogehoge", 22, "", "hogehoge", "~22"},
		{"HogeHoge.msnw", 22, "", "hogehoge", "~22"},
		{"db.hogehoge.msnw", 5432, "", "hogehoge", "db:5432"},
		{"10.0.0.7.hogehoge.msnw", 80, "", "hogehoge", "10.0.0.7:80"},
		// not an msnw name, no default exporter: direct (empty name)
		{"example.com", 443, "", "", "example.com:443"},
		{"10.0.0.1", 443, "", "", "10.0.0.1:443"},
		{"::1", 8080, "", "", "[::1]:8080"},
		{"localhost", 8765, "", "", "localhost:8765"},
		// with a default exporter they go through it
		{"example.com", 443, "exit", "exit", "example.com:443"},
		{"10.0.0.1", 443, "exit", "exit", "10.0.0.1:443"},
		{"localhost", 8765, "exit", "exit", "localhost:8765"},
	}
	for _, c := range cases {
		name, target := resolveHost(c.host, c.port, c.def)
		if name != c.name || target != c.target {
			t.Errorf("resolveHost(%q,%d,%q) = %q,%q; want %q,%q", c.host, c.port, c.def, name, target, c.name, c.target)
		}
	}
}
