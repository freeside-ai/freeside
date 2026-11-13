//go:build darwin

package macverify

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// hostSupported reports whether this host can realize the executor's
// confinements at all.
const hostSupported = true

// ephemeralPortFirst reads the first port of the host's ephemeral range,
// which reserved ports must stay below.
func ephemeralPortFirst() (int, error) {
	first, err := unix.SysctlUint32("net.inet.ip.portrange.first")
	if err != nil {
		return 0, fmt.Errorf("read net.inet.ip.portrange.first: %w", err)
	}
	return int(first), nil
}
