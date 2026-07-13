package golden_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// TestAssert is both the worked example every later unit copies and this
// unit's one golden-file test (issue #6, Acceptance 5). It renders a
// fixed value and asserts it against testdata/example.golden; run with
// -update to regenerate the fixture.
func TestAssert(t *testing.T) {
	got := []byte("freeside golden-file harness\nsecond line\n")
	golden.Assert(t, "example", got)
}
