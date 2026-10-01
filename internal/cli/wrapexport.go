package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// WrapExport runs a command and publishes the TCP port it listens on:
//
//	msnw wrap-export -key K -n foo -- python -m http.server
//
// The port is taken from -p, or from a "{port}" placeholder in the command
// (replaced by the chosen port; "-p 0" or no -p picks a free one), and is
// also passed to the command as $PORT. Without either, the port is detected
// by watching the command (and its children) for a listening socket, which
// is available on Linux. The export ends when the command exits.
func WrapExport(args []string) {
	fs := flag.NewFlagSet("msnw wrap-export", flag.ExitOnError)
	quietQUIC()
	name := fs.String("n", "", "name to export under (required)")
	port := fs.Int("p", -1, "port the command listens on; 0 = pick a free port (default: detect)")
	server := fs.String("s", envOr("MSNW_SERVER", DefaultServer), "rendezvous server host:port (or $MSNW_SERVER)")
	linkKey := fs.String("key", os.Getenv("MSNW_KEY"), "link key shared with clients (or $MSNW_KEY); required")
	serverKey := fs.String("server-key", os.Getenv("MSNW_SERVER_KEY"), "server key (or $MSNW_SERVER_KEY), if the server requires one")
	serverFP := fs.String("server-fp", os.Getenv("MSNW_SERVER_FP"), "pin the server's sha256 fingerprint (or $MSNW_SERVER_FP)")
	udpPort := fs.Int("port", 0, "local UDP port to bind (0 = random)")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Parse(args)
	cmdArgs := fs.Args()
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if *name == "" || *linkKey == "" || len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: msnw wrap-export -key LINKKEY -n NAME [-p PORT] -- COMMAND [ARGS...]")
		os.Exit(2)
	}

	// Decide the port up front when the command is told about it.
	placeholder := false
	for _, a := range cmdArgs {
		if strings.Contains(a, "{port}") {
			placeholder = true
		}
	}
	chosen := *port
	if placeholder && chosen < 0 {
		chosen = 0
	}
	if chosen == 0 {
		chosen = freePort()
	}
	if chosen > 0 {
		for i, a := range cmdArgs {
			cmdArgs[i] = strings.ReplaceAll(a, "{port}", strconv.Itoa(chosen))
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if chosen > 0 {
		cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(chosen))
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("%s: %v", cmdArgs[0], err)
	}
	exited := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = 1
		}
		exited <- code
	}()

	// Find the port, then export it until the command exits.
	ports := []int{}
	if chosen > 0 {
		ports = []int{chosen}
	} else {
		deadline := time.Now().Add(30 * time.Second)
		for len(ports) == 0 && time.Now().Before(deadline) {
			select {
			case code := <-exited:
				log.Printf("%s exited (%d) before listening on a port", cmdArgs[0], code)
				os.Exit(code)
			case <-time.After(200 * time.Millisecond):
			}
			ports = listeningPorts(cmd.Process.Pid)
		}
		if len(ports) == 0 {
			log.Printf("could not detect a listening port; use -p PORT or {port} in the command")
			cmd.Process.Signal(syscall.SIGTERM)
			os.Exit(<-exited)
		}
	}
	pol := &policy{}
	for _, p := range ports {
		pol.tcp = append(pol.tcp, target{host: "localhost", lo: p, hi: p})
	}
	log.Printf("exporting %q -> localhost:%d%s", *name, ports[0], extraPorts(ports))

	ectx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- serveExport(ectx, &exportOpts{server: *server, linkKey: *linkKey, serverKey: *serverKey,
			serverFP: *serverFP, udpPort: *udpPort, verbose: *verbose}, *name, pol)
	}()

	var code int
	select {
	case code = <-exited:
	case <-ctx.Done(): // interrupted: stop the command too
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case code = <-exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			code = <-exited
		}
	}
	cancel()
	<-done
	os.Exit(code)
}

func extraPorts(ports []int) string {
	if len(ports) < 2 {
		return ""
	}
	var s []string
	for _, p := range ports[1:] {
		s = append(s, strconv.Itoa(p))
	}
	return " (also " + strings.Join(s, ", ") + ")"
}

// freePort asks the kernel for an unused TCP port.
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
