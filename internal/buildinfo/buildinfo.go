// Package buildinfo reports the version of this binary. It is set by the
// build (-ldflags "-X .../internal/buildinfo.version=v1.2.3"), else taken
// from the module version Go recorded (go install), else "dev".
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var version = ""

// Version is the short version, e.g. "v0.1.5".
func Version() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// String is the long form printed by "msnw version".
func String() string {
	return fmt.Sprintf("msnw %s (%s, %s/%s)", Version(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Mismatch returns a note like " (exporter v0.1.4, this client v0.1.5)" when
// the peer's version differs from ours, or "" when it matches. An empty peer
// version (an older build that did not send one) shows as "unknown".
func Mismatch(peerRole, peerVersion, ownRole string) string {
	if peerVersion == "" {
		peerVersion = "unknown"
	}
	if peerVersion == Version() {
		return ""
	}
	return fmt.Sprintf(" (%s %s, this %s %s)", peerRole, peerVersion, ownRole, Version())
}
