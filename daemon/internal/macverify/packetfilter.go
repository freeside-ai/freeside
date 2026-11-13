package macverify

import (
	"bytes"
	"fmt"
	"slices"
)

// packetFilterHeader opens every rendered anchor, so the installed file says
// what wrote it.
const packetFilterHeader = "# freeside macverify: slot egress rules, written by freesided macverify install\n"

// PacketFilterRules renders the packet-filter anchor that drops every packet
// a slot account's sockets send or receive on any interface but loopback,
// the executor's second egress layer beside the sandbox profile (plan §5.7).
// Loopback is exempt (#3155) so a job can reach its own reserved ports on
// 127.0.0.1 and ::1; the sandbox profile alone decides which loopback ports
// a job may use, and pf still drops every packet bound off the host. It is the one renderer for
// both Config.PacketFilterRules and the anchor file install writes, so the
// bytes a conformance pass proves are the bytes the host loads.
//
// Rules name accounts, not uids: pf resolves the names when it loads the
// anchor, so the bytes depend only on the slot set and the configuration
// digest does not change with the ids an install picks.
func PacketFilterRules(slots []string) []byte {
	sorted := slices.Clone(slots)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var b bytes.Buffer
	b.WriteString(packetFilterHeader)
	for _, slot := range sorted {
		fmt.Fprintf(&b, "block drop out quick on ! lo0 user %s\n", slot)
		fmt.Fprintf(&b, "block drop in quick on ! lo0 user %s\n", slot)
	}
	return b.Bytes()
}
