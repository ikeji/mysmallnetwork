package cli

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Mosh runs mosh to a host published with "msnw export": it starts
// mosh-server over ssh (with this binary as the ProxyCommand), forwards the
// mosh UDP port through the tunnel in-process, and runs mosh-client against
// 127.0.0.1. The exporter must publish sshd as its default target and allow
// the mosh UDP ports, e.g. "msnw export -key K -n home -t 22 -u 60001-60999".
func Mosh(args []string) {
	fs := flag.NewFlagSet("msnw mosh", flag.ExitOnError)
	ports := fs.String("p", "60001:60999", "UDP port or range LO:HI for mosh-server (the exporter must allow it with -u)")
	sshOpts := fs.String("ssh", os.Getenv("MSNW_MOSH_SSH"), "extra options for the bootstrap ssh (or $MSNW_MOSH_SSH)")
	sshTimeout := fs.Duration("ssh-timeout", 60*time.Second, "give up on the bootstrap ssh after this long (0 = never)")
	nf := addNodeFlags(fs)
	pos := parseWithTrailingFlags(fs, args)
	if len(pos) != 1 || *nf.linkKey == "" {
		fmt.Fprintln(os.Stderr, "usage: msnw mosh -key LINKKEY [-p PORT|LO:HI] [-ssh OPTS] [-ssh-timeout D] [-v] [-log FILE] [user@]NAME")
		os.Exit(2)
	}
	dest := pos[0]
	name := dest[strings.LastIndex(dest, "@")+1:]
	if _, err := exec.LookPath("mosh-client"); err != nil {
		log.Fatal("mosh-client not found")
	}
	if err := nf.openLog("msnw mosh " + name); err != nil {
		log.Fatal(err)
	}

	// 1. Start mosh-server through ssh; it listens on loopback only.
	lang := os.Getenv("LANG")
	if lang == "" {
		lang = "en_US.UTF-8"
	}
	sshArgs, err := sshProxyArgs(nf, name)
	if err != nil {
		log.Fatal(err)
	}
	sshArgs = append(sshArgs, strings.Fields(*sshOpts)...)
	sshArgs = append(sshArgs, dest, "--", "mosh-server", "new", "-i", "127.0.0.1",
		"-p", *ports, "-c", "256", "-l", "LANG="+lang)
	ctx, cancel := context.WithCancel(context.Background())
	if *sshTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, *sshTimeout)
	}
	defer cancel()
	ssh := exec.CommandContext(ctx, "ssh", sshArgs...)
	ssh.Stderr = os.Stderr
	ssh.WaitDelay = 2 * time.Second // do not wait on a grandchild that still holds the pipe
	out, err := ssh.Output()
	if ctx.Err() == context.DeadlineExceeded {
		// ssh's own ConnectTimeout does not cover a ControlMaster socket
		// whose master no longer answers, and ssh then waits forever.
		log.Fatalf("ssh bootstrap timed out after %s (a stale ControlMaster may be in use; try \"ssh -O exit %s\", or -ssh-timeout 0)", *sshTimeout, dest)
	}
	if err != nil {
		log.Fatalf("ssh bootstrap failed: %v", err)
	}
	sport, mkey := "", ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 4 && f[0] == "MOSH" && f[1] == "CONNECT" {
			sport, mkey = f[2], f[3]
		}
	}
	if mkey == "" {
		log.Fatal("mosh-server did not start (no MOSH CONNECT line)")
	}

	// 2. Forward a random local UDP port to the exporter's port, in this
	// process. A random port avoids clashing with local mosh-servers or a
	// second msnw mosh session.
	p, stop, err := newPool(nf)
	if err != nil {
		log.Fatal(err)
	}
	defer stop()
	uc, err := listenUDP("127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go serveUDPForward(p.ctx, p, name, sport, uc)
	lport := strconv.Itoa(uc.LocalAddr().(*net.UDPAddr).Port)

	// 3. Run mosh-client against the local forwarder.
	mc := exec.Command("mosh-client", "127.0.0.1", lport)
	mc.Env = append(os.Environ(), "MOSH_KEY="+mkey)
	mc.Stdin, mc.Stdout, mc.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := mc.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			stop()
			os.Exit(ee.ExitCode())
		}
		log.Fatal(err)
	}
}
