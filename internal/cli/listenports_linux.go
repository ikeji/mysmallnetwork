package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// listeningPorts returns the TCP ports on which pid or any of its descendants
// is listening, found through /proc: LISTEN sockets in /proc/net/tcp{,6} are
// matched by inode against the processes' open file descriptors.
func listeningPorts(pid int) []int {
	pids := descendants(pid)
	inodes := map[string]bool{}
	for _, p := range pids {
		fds, _ := os.ReadDir("/proc/" + strconv.Itoa(p) + "/fd")
		for _, fd := range fds {
			if link, err := os.Readlink("/proc/" + strconv.Itoa(p) + "/fd/" + fd.Name()); err == nil &&
				strings.HasPrefix(link, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	seen := map[int]bool{}
	var ports []int
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// sl local_address rem_address st ... inode
			if len(fields) < 10 || fields[3] != "0A" || !inodes[fields[9]] {
				continue
			}
			i := strings.LastIndex(fields[1], ":")
			port, err := strconv.ParseInt(fields[1][i+1:], 16, 32)
			if err != nil || seen[int(port)] {
				continue
			}
			seen[int(port)] = true
			ports = append(ports, int(port))
		}
		f.Close()
	}
	sort.Ints(ports)
	return ports
}

// descendants returns pid and all processes below it.
func descendants(pid int) []int {
	parent := map[int]int{}
	entries, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ..." — comm may contain spaces, so cut at the last ')'
		s := string(b)
		rp := strings.LastIndex(s, ")")
		if rp < 0 {
			continue
		}
		rest := strings.Fields(s[rp+1:])
		if len(rest) < 2 {
			continue
		}
		p, _ := strconv.Atoi(strings.TrimSuffix(filepath.Base(filepath.Dir(e)), ""))
		pp, _ := strconv.Atoi(rest[1])
		parent[p] = pp
	}
	out := []int{pid}
	for changed := true; changed; {
		changed = false
		for p, pp := range parent {
			if contains(out, pp) && !contains(out, p) {
				out = append(out, p)
				changed = true
			}
		}
	}
	return out
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
