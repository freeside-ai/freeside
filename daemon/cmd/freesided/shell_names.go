package main

import (
	"net/netip"

	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
)

// These forwards keep the shell names that Tier A files read resolvable
// under their old spelling, so the shell's move out of package main (#2861)
// changes no Tier A file. Each forward goes when its reader next changes for
// another reason: serverShutdownBudget for pairing_control.go,
// readTailscaleIPs for prelaunch.go, and loopbackTwinListenAddress for
// preflight.go.

const serverShutdownBudget = daemonshell.ServerShutdownBudget

func readTailscaleIPs() ([]netip.Addr, error) { return daemonshell.ReadTailscaleIPs() }

func loopbackTwinListenAddress(address string) (string, bool, error) {
	return daemonshell.LoopbackTwinListenAddress(address)
}
