package publish

import "testing"

// This file exposes in-package test fixtures to the external
// publish_test package. Its symbols are compiled into the test binary
// only, so nothing here widens the package's production surface: the
// file scheme stays unreachable through NewTransport (validRemoteBase),
// and GatedHead stays unmintable outside a real publication gate.

// LocalRemoteFixture is a bare managed repository reachable over the
// file scheme, with one commit on main, plus the Transport bound to it.
// The composed gate test needs a Transport that really pushes and a ref
// namespace it can inspect afterwards; the production option surface
// cannot select the file scheme, which is exactly the point.
type LocalRemoteFixture struct {
	Transport *Transport
	Repo      string
	Bare      string
	BaseSHA   string
}

// NewLocalRemoteFixture builds the local remote and its transport under
// the given owner/name, so the caller's other fixtures (forge fake,
// trust profile, authorization) can name that same repository.
func NewLocalRemoteFixture(t *testing.T, repo string) LocalRemoteFixture {
	t.Helper()
	remote := newLocalRemoteForRepo(t, repo)
	return LocalRemoteFixture{
		Transport: remote.transport,
		Repo:      remote.repo,
		Bare:      remote.bare,
		BaseSHA:   remote.baseSHA,
	}
}

// CandidateHeadForTest writes a daemon-style plumbing commit onto the
// base in a fetched checkout and returns its SHA, standing in for the
// candidate head the importer would have authored.
func CandidateHeadForTest(t *testing.T, co Checkout) string {
	t.Helper()
	return candidateHead(t, co)
}

// GateHeadForTest mints the publication gate capability that owner will
// accept, outside a real Publisher, for tests that drive the transport
// directly rather than through Publish. Tests of the composition itself
// must take the capability from Publisher's callback, which is the only
// production mint site.
func GateHeadForTest(t *testing.T, owner *Transport, in IdentityInput) GatedHead {
	t.Helper()
	return testGatedHead(t, owner, in)
}
