package main

import (
	"runtime/debug"
)

// version is set by release or operator builds with -ldflags -X. Local builds
// fall back to the module version embedded by the Go toolchain.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "devel"
}
