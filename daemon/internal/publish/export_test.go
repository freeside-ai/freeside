package publish

import "testing"

// This file exposes in-package test fixtures to the external
// publish_test package. Its symbols are compiled into the test binary
// only, so nothing here widens the package's production surface: the
// file scheme stays unreachable through NewTransport (validRemoteBase),
// and GatedHead stays unmintable outside a real publication gate.

// GateHeadForTest mints the publication gate capability that owner will
// accept, outside a real Publisher, for tests that drive the transport
// directly rather than through Publish. Tests of the composition itself
// must take the capability from Publisher's callback, which is the only
// production mint site.
func GateHeadForTest(t *testing.T, owner *Transport, in IdentityInput) GatedHead {
	t.Helper()
	return testGatedHead(t, owner, in)
}
