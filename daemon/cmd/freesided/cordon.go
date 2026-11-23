package main

import (
	"fmt"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
)

// runCordonMain answers the cordon's hidden verbs. The only one is probe:
// the conformance probe a cordoned probe launch runs, from a copy of this
// binary, so the probe is the build the conformance record names. It needs
// no daemon root and prints only its report.
func runCordonMain(args []string) {
	if len(args) > 0 && args[0] == "probe" {
		os.Exit(cordon.RunProbe(args[1:], os.Stdout))
	}
	fmt.Fprintln(os.Stderr, "usage: freesided cordon probe <spec-json>")
	os.Exit(2)
}
