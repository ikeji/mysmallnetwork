package cli

import (
	"flag"
	"reflect"
	"testing"
)

func TestServerFlagsResolve(t *testing.T) {
	saved := DefaultServers
	DefaultServers = []DefaultServer{{Addr: "d0:4433", FP: "aa"}, {Addr: "d1:4433", FP: ""}, {Addr: "d2:4433", FP: "cc"}}
	defer func() { DefaultServers = saved }()

	cases := []struct {
		name         string
		args         []string
		env, envFP   string
		servers, fps []string
	}{
		{"defaults", nil, "", "", []string{"d0:4433", "d1:4433", "d2:4433"}, []string{"aa", "cc"}},
		{"default servers, explicit pin", []string{"-server-fp", "zz"}, "", "", []string{"d0:4433", "d1:4433", "d2:4433"}, []string{"zz"}},
		{"-s drops default pins", []string{"-s", "x:1"}, "", "", []string{"x:1"}, nil},
		{"-s repeated and comma", []string{"-s", "x:1,y:2", "-s", "z:3"}, "", "", []string{"x:1", "y:2", "z:3"}, nil},
		{"env", nil, "x:1, y:2", "f1,f2", []string{"x:1", "y:2"}, []string{"f1", "f2"}},
		{"-s beats env", []string{"-s", "z:3"}, "x:1", "f1", []string{"z:3"}, []string{"f1"}},
	}
	for _, c := range cases {
		t.Setenv("MSNW_SERVER", c.env)
		t.Setenv("MSNW_SERVER_FP", c.envFP)
		fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
		sf := addServerFlags(fs)
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		sf.resolve()
		if !reflect.DeepEqual([]string(sf.servers), c.servers) || !reflect.DeepEqual([]string(sf.fps), c.fps) {
			t.Errorf("%s: servers=%v fps=%v; want %v %v", c.name, sf.servers, sf.fps, c.servers, c.fps)
		}
	}
}
