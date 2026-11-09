//go:build !darwin

package macverify

// NewSystemHost refuses off macOS: the executor's host state is macOS
// accounts, launchd, and pf.
func NewSystemHost() (Host, error) { return nil, ErrHostUnsupported }
