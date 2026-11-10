package daemoncompose

import (
	"crypto/rand"
	"crypto/rsa"
	"sync"
	"testing"
)

// sharedSetupTestKey generates one RSA key per test binary, on first use.
// It is lazy, not a package variable, because TestDaemonRecoversAcrossSIGKILL
// starts this binary as its daemon, and each start would pay for the key.
var sharedSetupTestKey = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

// setupTestKey returns the shared test key. Callers that save several apps
// get the same key for each; nothing under test requires distinct keys.
func setupTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := sharedSetupTestKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}
