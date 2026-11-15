package cli

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSSHProxyArgsVerbose(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("MSNW_KEY", "testkey")
	t.Setenv("MSNW_LOG", "")

	cases := []struct {
		args      []string
		wantProxy bool
		wantSSH   bool
	}{
		{[]string{"user@home"}, false, false},
		{[]string{"-v", "user@home"}, true, false},
		{[]string{"-vv", "user@home"}, true, true},
		{[]string{"-v", "-v", "user@home"}, true, true},
		{[]string{"user@home", "-vv"}, true, true},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		nf := addNodeFlags(fs)
		addSSHVerboseFlag(fs, nf)
		pos := parseWithTrailingFlags(fs, c.args)
		if !slices.Equal(pos, []string{"user@home"}) {
			t.Fatalf("args %v: pos = %v, want [user@home]", c.args, pos)
		}
		got, err := sshProxyArgs(nf, "home")
		if err != nil {
			t.Fatalf("args %v: sshProxyArgs: %v", c.args, err)
		}
		if len(got) < 2 || got[0] != "-o" {
			t.Fatalf("args %v: unexpected ssh args %v", c.args, got)
		}
		hasProxyV := strings.HasSuffix(got[1], " -v")
		hasSSHV := slices.Contains(got[2:], "-v")
		if hasProxyV != c.wantProxy || hasSSHV != c.wantSSH {
			t.Errorf("args %v: got %v (proxy -v = %v, ssh -v = %v), want (%v, %v)",
				c.args, got, hasProxyV, hasSSHV, c.wantProxy, c.wantSSH)
		}
	}
}
