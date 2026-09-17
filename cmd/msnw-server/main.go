// msnw-server: rendezvous + relay server for msnw.
package main

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

func main() {
	// quic-go warns loudly when the UDP receive buffer is small; the warning is
	// harmless for a tunnel of this size, and sysctl advice lives in the README.
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	}
	listen := flag.String("listen", ":4433", "control listen address (UDP/QUIC)")
	relay := flag.String("relay", ":4434", "relay listen address (UDP); must be reachable by nodes")
	serverKey := flag.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key nodes must present (or $MSNW_SERVER_KEY); empty = open server")
	key := flag.String("key", "", "path to persistent private key (created if missing); default: ephemeral")
	flag.Parse()
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
