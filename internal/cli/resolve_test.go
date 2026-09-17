package cli

import "testing"

func TestResolveHost(t *testing.T) {
	cases := []struct {
		host              string
		port              int
		def, name, target string
		ok                bool
	}{
		{"hogehoge", 22, "", "hogehoge", "22", true},
		{"HogeHoge.msnw", 22, "", "hogehoge", "22", true},
		{"db.hogehoge.msnw", 5432, "", "hogehoge", "db:5432", true},
		{"10.0.0.7.hogehoge.msnw", 80, "", "hogehoge", "10.0.0.7:80", true},
		{"example.com", 443, "", "", "", false},
		{"10.0.0.1", 443, "", "", "", false},
		{"example.com", 443, "exit", "exit", "example.com:443", true},
		{"10.0.0.1", 443, "exit", "exit", "10.0.0.1:443", true},
	}
	for _, c := range cases {
		name, target, err := resolveHost(c.host, c.port, c.def)
		if (err == nil) != c.ok || name != c.name || target != c.target {
			t.Errorf("resolveHost(%q,%d,%q) = %q,%q,%v; want %q,%q ok=%v", c.host, c.port, c.def, name, target, err, c.name, c.target, c.ok)
		}
	}
}
