package macverify

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Reserved ports (#3155, plan §5.7 macOS Execution). Each slot has a fixed
// set of portsPerSlot ports a job may bind and connect on, over TCP and UDP,
// on the host's own addresses; the sandbox profile denies every other port.
// Slot i in sorted order holds base+portsPerSlot*i through
// base+portsPerSlot*i+portsPerSlot-1, so the sets are disjoint by
// construction. The rules every set must meet: each port at or above
// minReservedPort, every set below the host's ephemeral range (so the kernel
// never hands a reserved port to an unrelated outbound socket), and none the
// daemon listens on (daemoncompose refuses that). The pre-job listener check
// refuses a job while anything else holds one of its ports.

const (
	// portsPerSlot is how many ports each slot reserves. The convergence
	// harness needs three (API, control, and an ntfy URL nothing answers);
	// the fourth is headroom.
	portsPerSlot = 4
	// minReservedPort keeps reservations off the privileged ports.
	minReservedPort = 1024
	// DefaultPortBase is the first reserved port when the operator names
	// none: below macOS's default ephemeral range (49152) with room for
	// hundreds of slots.
	DefaultPortBase = 47000
)

// ErrReservedPortBusy refuses a job while something on the host listens on,
// or has bound, one of its slot's reserved ports. The slot is released, not
// quarantined: a host listener is not a broken confinement.
var ErrReservedPortBusy = errors.New("macverify: a reserved port is in use")

// slotPortSet is one slot's reserved ports, ascending.
type slotPortSet [portsPerSlot]int

// String renders the set as FREESIDE_VERIFY_PORTS and slot-run report it:
// the ports comma-separated, ascending.
func (s slotPortSet) String() string {
	parts := make([]string, len(s))
	for i, p := range s {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// reservedPorts assigns each of slots, sorted and unique, its set from base.
func reservedPorts(base int, slots []string) map[string]slotPortSet {
	sets := make(map[string]slotPortSet, len(slots))
	for i, slot := range slots {
		var set slotPortSet
		for k := range set {
			set[k] = base + portsPerSlot*i + k
		}
		sets[slot] = set
	}
	return sets
}

// validatePortBase refuses a base whose sets for n slots break a rule:
// a port below minReservedPort or at or above ephemeralFirst.
func validatePortBase(base, n, ephemeralFirst int) error {
	switch {
	case base < minReservedPort:
		return fmt.Errorf("port base %d is below %d", base, minReservedPort)
	case base >= ephemeralFirst:
		// Checked before the sum below, which a huge base would overflow.
		return fmt.Errorf("port base %d is in the ephemeral range, which starts at %d", base, ephemeralFirst)
	}
	if last := base + portsPerSlot*n - 1; last >= ephemeralFirst {
		return fmt.Errorf("the reserved ports %d-%d reach the ephemeral range, which starts at %d", base, last, ephemeralFirst)
	}
	return nil
}

// ReservesPort reports whether port is in any slot's reserved set.
func (c Config) ReservesPort(port int) bool {
	last := c.PortBase + portsPerSlot*len(c.Slots) - 1
	return port >= c.PortBase && port <= last
}

// RenderPortsConf renders the bin/ports.conf install writes and slot-run
// reads: one line per slot, `<slot> <p1> <p2> <p3> <p4>`, the slots sorted.
func RenderPortsConf(base int, slots []string) []byte {
	sorted := slices.Sorted(slices.Values(slots))
	sets := reservedPorts(base, sorted)
	var b strings.Builder
	for _, slot := range sorted {
		b.WriteString(slot)
		for _, p := range sets[slot] {
			b.WriteString(" " + strconv.Itoa(p))
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}
