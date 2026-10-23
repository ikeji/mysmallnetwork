package cli

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// SSH runs ssh to a host published with "msnw export", with this binary as
// the ProxyCommand: msnw ssh [user@]NAME[:port] [COMMAND...]. Everything
// after the name is the remote command, as with ssh itself; ssh options go
// in -ssh (or $MSNW_SSH). NAME is also what ssh shows and looks up in
// ~/.ssh/config; the ProxyCommand makes the actual path.
func SSH(args []string) {
	fs := flag.NewFlagSet("msnw ssh", flag.ExitOnError)
	sshOpts := fs.String("ssh", os.Getenv("MSNW_SSH"), "extra options for ssh (or $MSNW_SSH)")
	nf := addNodeFlags(fs)
	fs.Parse(args)
	if fs.NArg() < 1 || *nf.linkKey == "" {
		fmt.Fprintln(os.Stderr, "usage: msnw ssh -key LINKKEY [-ssh OPTS] [-v] [-log FILE] [user@]NAME[:port|:host:port] [COMMAND...]")
		os.Exit(2)
	}
	dest, command := fs.Arg(0), fs.Args()[1:]
	user, spec := "", dest
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		user, spec = dest[:i+1], dest[i+1:]
	}
	name := spec
	if i := strings.Index(spec, ":"); i >= 0 {
		name = spec[:i]
	}
	if name == "" {
		log.Fatalf("bad destination %q", dest)
	}
	if err := nf.openLog("msnw ssh " + name); err != nil {
		log.Fatal(err)
	}
	sshArgs, err := sshProxyArgs(nf, spec)
	if err != nil {
		log.Fatal(err)
	}
	sshArgs = append(sshArgs, strings.Fields(*sshOpts)...)
	sshArgs = append(sshArgs, user+name)
	if len(command) > 0 {
		sshArgs = append(sshArgs, "--")
		sshArgs = append(sshArgs, command...)
	}
	ssh := exec.Command("ssh", sshArgs...)
	ssh.Stdin, ssh.Stdout, ssh.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := ssh.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		log.Fatal(err)
	}
}

// sshProxyArgs returns the leading ssh arguments that route the connection
// through this binary: a ProxyCommand running "msnw connect spec" with the
// same -v and -log, and -v for ssh itself when verbose. The link key and
// server list go to the child through the environment, so that they do not
// show up in the command line. Without -v, ssh discards the ProxyCommand's
// stderr whenever ControlPersist is on, so -v is the way to see why a
// connection failed.
func sshProxyArgs(nf *nodeFlags, spec string) ([]string, error) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, fmt.Errorf("ssh not found")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	nf.exportEnv()
	proxy := fmt.Sprintf("ProxyCommand=%s connect %s", shellQuote(self), shellQuote(spec))
	if *nf.verbose {
		proxy += " -v"
	}
	if *nf.logFile != "" {
		proxy += " -log " + shellQuote(*nf.logFile)
	}
	args := []string{"-o", proxy}
	if *nf.verbose {
		args = append(args, "-v")
	}
	return args, nil
}

// shellQuote makes s safe inside ssh's ProxyCommand, which the user's shell
// interprets.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
