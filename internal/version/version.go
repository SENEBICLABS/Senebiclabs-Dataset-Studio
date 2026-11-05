// Package version reports build identity for logs, CLI output and, later,
// reproducibility metadata.
package version

import (
	"runtime"
	"runtime/debug"
)

// Version and Commit are overridden at build time with -ldflags -X.
var (
	Version = "dev"
	Commit  = "unknown"
)

// Info is the build identity of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	Module    string `json:"module"`
}

// Get returns the build identity. When Commit was not injected it falls back
// to the VCS revision recorded by the Go toolchain, if any.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info.Module = bi.Main.Path
		if info.Commit == "unknown" {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					info.Commit = s.Value
				}
			}
		}
	}
	return info
}
