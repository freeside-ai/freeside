package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
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

// cordonBackendFlags registers the cordoned launch backend's flags, which
// take effect with -driver claude. An empty -cordon-agent-cli configures
// none.
func cordonBackendFlags(flags *flag.FlagSet) func() daemoncompose.CordonBackendConfig {
	agentCLI := flags.String("cordon-agent-cli", "",
		"with -driver claude: absolute path of the pinned agent CLI a cordoned launch runs; empty (default) configures no cordon")
	agentCLISHA256 := flags.String("cordon-agent-cli-sha256", "",
		"SHA-256 of -cordon-agent-cli in lowercase hex; required with it")
	projects := flags.String("cordon-projects", "",
		"directory holding the owner's other projects, whose .env files a launch can't read; required with -cordon-agent-cli")
	var protectedRoots repeatedStringFlag
	flags.Var(&protectedRoots, "cordon-protected-root",
		"directory a cordoned launch can't list, read, write, or reach a socket under, besides -state-dir; repeatable")
	stallLimit := flags.Duration("cordon-stall-limit", 10*time.Minute,
		"end a cordoned launch whose output has been silent this long")
	endGrace := flags.Duration("cordon-end-grace", 10*time.Second,
		"time a cordoned launch being ended has between SIGTERM and SIGKILL")
	endProofTimeout := flags.Duration("cordon-end-proof-timeout", time.Minute,
		"bound on the proof that no process of a cordoned launch is alive; a launch it can't prove ended is blocked")
	return func() daemoncompose.CordonBackendConfig {
		return daemoncompose.CordonBackendConfig{
			AgentCLI: *agentCLI, AgentCLISHA256: *agentCLISHA256, Projects: *projects,
			ProtectedRoots: protectedRoots,
			StallLimit:     *stallLimit, EndGrace: *endGrace, EndProofTimeout: *endProofTimeout,
		}
	}
}
