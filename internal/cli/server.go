// Package cli implements the msnw subcommands.
package cli

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mysmallnetwork/internal/ident"
	"mysmallnetwork/internal/rendezvous"
)

// Server runs the rendezvous + relay server.
func Server(args []string) {
	fs := flag.NewFlagSet("msnw server", flag.ExitOnError)
	quietQUIC()
	listen := fs.String("listen", ":4433", "control listen address (UDP/QUIC)")
	relay := fs.String("relay", ":4434", "relay listen address (UDP); must be reachable by nodes")
	serverKey := fs.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key nodes must present (or $MSNW_SERVER_KEY); empty = open server")
	key := fs.String("key", "", "path to persistent private key (created if missing); default: ephemeral")
	fs.Parse(args)
	if *serverKey == "" {
		log.Print("no -server-key set: this is an open server (link keys still protect every tunnel)")
	}
	var id *ident.Identity
	var err error
	if *key != "" {
		id, err = ident.Load(*key)
	} else {
		id, err = ident.New()
	}
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &rendezvous.Server{Ident: id, ServerKey: *serverKey}
	if err := srv.Run(ctx, *listen, *relay); err != nil {
		log.Fatal(err)
	}
}
