// msnw: one binary with subcommands.
//
//	msnw server   [-server-key S] [-listen :4433] [-relay :4434] [-key file]
//	msnw export   -key K -n NAME -t [host:]port [-t ...] [--all]
//	msnw client   -key K -n NAME[:port] [-l [addr]] | --socks5 [addr]
//	msnw mosh     -key K [-p PORT] [user@]NAME
//	msnw gen-key
//	msnw version
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"

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
  version   print the version

Run "msnw <command> -h" for the options of a command.
`

// version is set by the release build (-ldflags "-X main.version=v1.2.3");
// otherwise it falls back to the module version Go recorded, if any.
var version = ""

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	return fmt.Sprintf("msnw %s (%s, %s/%s)", v, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

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
	case "version", "-V", "--version":
		fmt.Println(versionString())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "msnw: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
