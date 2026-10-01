//go:build !linux

package cli

// listeningPorts cannot inspect sockets of other processes here; wrap-export
// then needs -p or a {port} placeholder.
func listeningPorts(pid int) []int { return nil }
