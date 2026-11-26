//go:build darwin && cgo

package cordon

import "testing"

// TestHostFactsAreReadable shows the real reader answers on this host. It
// can't say what the answer should be: that depends on what the process
// running the tests was approved for.
func TestHostFactsAreReadable(t *testing.T) {
	t.Parallel()
	facts, err := readHostFacts()
	if err != nil {
		t.Fatalf("readHostFacts: %v", err)
	}
	t.Logf("host facts: %+v", facts)
}
