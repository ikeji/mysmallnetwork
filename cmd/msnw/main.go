// msnw: one binary with subcommands.
//
//	msnw server   [-server-key S] [-listen :4433] [-relay :4434] [-key file]
//	msnw export   -key K -n NAME -t [host:]port [-t ...] [--all]
//	msnw wrap-export -key K -n NAME [-p PORT] -- COMMAND [ARGS...]
//	msnw import   -key K -l [addr] NAME[:port]
//	msnw connect  -key K NAME[:port]
//	msnw socks5-proxy | http-proxy | proxy
//	msnw mosh     -key K [-p PORT] [user@]NAME
//	msnw gen-key
//	msnw version
package main

import (
	"fmt"
	"os"

	"github.com/ikeji/mysmallnetwork/internal/buildinfo"
	"github.com/ikeji/mysmallnetwork/internal/cli"
	"github.com/ikeji/mysmallnetwork/internal/ident"
)

const usage = `usage: msnw <command> [options]

commands:
  server    run the rendezvous + relay server
  export    publish local services under a name
  wrap-export  run a command and publish the port it listens on
  import    bring a published port to a local port (-l [addr] NAME[:port])
  connect   pipe stdin/stdout to a published port (nc style; ssh ProxyCommand)
  socks5-proxy   SOCKS5 proxy on 127.0.0.1:1080 (msnw names via the tunnel)
  http-proxy     HTTP proxy on 127.0.0.1:8080
  proxy          both proxies in one process
  mosh      run mosh to a published host (ssh + UDP through the tunnel)
  gen-key   print a fresh random link key
  version   print the version

Run "msnw <command> -h" for the options of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "server":
		cli.Server(args)
	case "export", "exporter":
		cli.Export(args)
	case "wrap-export", "wrap":
		cli.WrapExport(args)
	case "import":
		cli.Import(args)
	case "connect":
		cli.Connect(args)
	case "socks5-proxy", "socks5":
		cli.Socks5Proxy(args)
	case "http-proxy":
		cli.HTTPProxy(args)
	case "proxy":
		cli.Proxy(args)
	case "mosh":
		cli.Mosh(args)
	case "gen-key", "genkey":
		fmt.Println(ident.GenerateKey())
	case "version", "-V", "--version":
		fmt.Println(buildinfo.String())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "msnw: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
