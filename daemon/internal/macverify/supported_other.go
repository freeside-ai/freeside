//go:build !darwin

package macverify

// hostSupported reports whether this host can realize the executor's
// confinements at all. They are macOS mechanisms, so nowhere else.
const hostSupported = false

// ephemeralPortFirst is the IANA ephemeral range's first port, macOS's
// default. Off macOS the executor never runs a job, so this only lets a
// configuration validate the same way in tests.
func ephemeralPortFirst() (int, error) { return 49152, nil }
