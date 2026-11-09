//go:build !darwin

package macverify

// hostSupported reports whether this host can realize the executor's
// confinements at all. They are macOS mechanisms, so nowhere else.
const hostSupported = false
