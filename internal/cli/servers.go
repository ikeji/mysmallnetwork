package cli

import (
	"flag"
	"os"
	"strings"

	"github.com/ikeji/mysmallnetwork/internal/ident"
	"github.com/ikeji/mysmallnetwork/internal/peer"
)

// DefaultServer is one public rendezvous server and the fingerprint of its
// identity (the "fingerprint: sha256:..." line it prints at startup when run
// with -identity). An empty FP means the server is not verified.
type DefaultServer struct {
	Addr, FP string
}

// DefaultServers are the public rendezvous servers used when neither -s nor
// $MSNW_SERVER is given, so a downloaded binary works without running a
// server. Nodes use all of them at once, so any one of them may be down.
var DefaultServers = []DefaultServer{
	{Addr: "relay0.ikeji.ma:4433", FP: "d61823b97116661825c109f1a2f848c477db7ffc483c92562a6fec42399b9bce"},
	{Addr: "relay1.ikeji.ma:4433", FP: "f0696909d1fdd2a3049bbbf9ed497e2e7e3a8e6abdf8ccf6fdd8e52689441309"},
	{Addr: "relay2.ikeji.ma:4433", FP: "2e2a584206a0824947e2037f1c1cc7eefc7978ad23f16f951f742e85eb80c285"},
}

// listFlag is a repeatable flag whose values may also be comma-separated,
// so "-s a -s b" and "-s a,b" and MSNW_SERVER=a,b all mean the same.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, splitList(v)...)
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// serverFlags are the rendezvous server options shared by every node
// subcommand: -s / $MSNW_SERVER, -server-key / $MSNW_SERVER_KEY and
// -server-fp / $MSNW_SERVER_FP.
type serverFlags struct {
	servers, fps listFlag
	key          *string
	resolved     bool
}

func addServerFlags(fs *flag.FlagSet) *serverFlags {
	sf := &serverFlags{}
	fs.Var(&sf.servers, "s", "rendezvous server host:port, repeatable or comma-separated (or $MSNW_SERVER; default: the public servers)")
	sf.key = fs.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key (or $MSNW_SERVER_KEY), if the servers require one")
	fs.Var(&sf.fps, "server-fp", "accept only servers with one of these sha256 fingerprints, repeatable or comma-separated (or $MSNW_SERVER_FP)")
	return sf
}

// resolve fills in the environment and the defaults after flag parsing:
// -s beats $MSNW_SERVER beats DefaultServers, and -server-fp beats
// $MSNW_SERVER_FP beats the fingerprints of DefaultServers, which only
// apply when the default servers are in use. It is idempotent; exportEnv
// and newNode call it themselves.
func (sf *serverFlags) resolve() {
	if sf.resolved {
		return
	}
	sf.resolved = true
	if len(sf.servers) == 0 {
		sf.servers = splitList(os.Getenv("MSNW_SERVER"))
	}
	if len(sf.fps) == 0 {
		sf.fps = splitList(os.Getenv("MSNW_SERVER_FP"))
	}
	if len(sf.servers) == 0 {
		useDefaultFPs := len(sf.fps) == 0
		for _, d := range DefaultServers {
			sf.servers = append(sf.servers, d.Addr)
			if useDefaultFPs && d.FP != "" {
				sf.fps = append(sf.fps, d.FP)
			}
		}
	}
}

// exportEnv puts the effective server options into the environment so that
// child processes inherit them.
func (sf *serverFlags) exportEnv() {
	sf.resolve()
	os.Setenv("MSNW_SERVER", sf.servers.String())
	os.Setenv("MSNW_SERVER_KEY", *sf.key)
	os.Setenv("MSNW_SERVER_FP", sf.fps.String())
}

// newNode binds the shared socket for the configured servers.
func (sf *serverFlags) newNode(id *ident.Identity, port int) (*peer.Node, error) {
	sf.resolve()
	return peer.New(id, sf.servers, *sf.key, sf.fps, port)
}
