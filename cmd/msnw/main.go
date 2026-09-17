// msnw: one binary with subcommands.
//
//	msnw server   [-server-key S] [-listen :4433] [-relay :4434] [-key file]
//	msnw export   -key K -n NAME -t [host:]port [-t ...] [--all]
//	msnw client   -key K -n NAME[:port] [-l [addr]] | --socks5 [addr]
//	msnw mosh     -key K [-p PORT] [user@]NAME
//	msnw gen-key
package main

import (
	"fmt"
	"os"

	"github.com/ikeji/mysmallnetwork/internal/cli"
	"github.com/ikeji/mysmallnetwork/internal/ident"
)

const usage = `usage: msnw <command> [options]

commands:
  server    run the rendezvous + relay server
  export    publish local services under a name
  client    reach a published service (stdio, -l local port, --socks5)
  mosh      run mosh to a published host (ssh + UDP through the tunnel)
  gen-key   print a fresh random link key

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
	case "client":
		cli.Client(args)
	case "mosh":
		cli.Mosh(args)
	case "gen-key", "genkey":
		fmt.Println(ident.GenerateKey())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "msnw: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
