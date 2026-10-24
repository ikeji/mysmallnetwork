package netutil

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseForward(t *testing.T) {
	cases := []struct {
		in   string
		want Forward
	}{
		{"felsi", Forward{Host: "127.0.0.1", Name: "felsi"}},
		{"felsi:443", Forward{Host: "127.0.0.1", Port: 443, Name: "felsi", Target: "443"}},
		{"5000=felsi", Forward{Host: "127.0.0.1", Port: 5000, Name: "felsi"}},
		{":5000=felsi", Forward{Host: "", Port: 5000, Name: "felsi"}},
		{"0.0.0.0:443=felsi", Forward{Host: "0.0.0.0", Port: 443, Name: "felsi"}},
		{"10.0.0.1:443=felsi:8443", Forward{Host: "10.0.0.1", Port: 443, Name: "felsi", Target: "8443"}},
		{"8080=exit@example.com:80", Forward{Host: "127.0.0.1", Port: 8080, Name: "exit", Target: "example.com:80"}},
		{"exit@example.com:80", Forward{Host: "127.0.0.1", Port: 80, Name: "exit", Target: "example.com:80"}},
		{"[::1]:443=felsi:443", Forward{Host: "::1", Port: 443, Name: "felsi", Target: "443"}},
		{"80=exit@[2001:db8::1]:80", Forward{Host: "127.0.0.1", Port: 80, Name: "exit", Target: "[2001:db8::1]:80"}},
		{"felsi:60001/udp", Forward{Host: "127.0.0.1", Port: 60001, UDP: true, Name: "felsi", Target: "60001"}},
		{"10.0.0.1:443=felsi:443/tcp", Forward{Host: "10.0.0.1", Port: 443, Name: "felsi", Target: "443"}},
	}
	for _, c := range cases {
		got, err := ParseForward(c.in)
		if err != nil {
			t.Errorf("ParseForward(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseForward(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for in, msg := range map[string]string{
		"":                     "missing exporter name",
		"5000=":                "missing exporter name",
		"=felsi":               "local side",
		"felsi=felsi":          "bad port", // left of = is always local, so this is a port that is not one
		"1.2.3.4=felsi":        "bad port", // an address needs its port
		":x=felsi":             "bad port",
		"felsi:x":              "bad port",
		"felsi:70000":          "bad port",
		"felsi:example.com:80": "bad port", // the any-host form takes @
		"exit@example.com":     "not host:port",
		"exit@:80":             "not host:port",
		"@example.com:80":      "missing exporter name",
	} {
		_, err := ParseForward(in)
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("ParseForward(%q) err = %v, want %q", in, err, msg)
		}
	}
	f := Forward{Host: "", Port: 0, Name: "n", Target: "h:1"}
	if f.Listen(80) != ":80" || f.Remote() != "n@h:1" || (Forward{Name: "n"}).Remote() != "n" {
		t.Errorf("Listen/Remote: %q %q", f.Listen(80), f.Remote())
	}
}

func TestSplitName(t *testing.T) {
	for _, c := range [][3]string{{"a", "a", ""}, {"a:22", "a", "22"}, {"a@h:22", "a", "h:22"}, {"a@[::1]:22", "a", "[::1]:22"}} {
		n, tg := SplitName(c[0])
		if n != c[1] || tg != c[2] {
			t.Errorf("SplitName(%q) = %q,%q", c[0], n, tg)
		}
	}
}
