package fake_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// acceptor is the test-local at-most-one-accepted check (§5.3): drivers
// re-deliver committed results on every Collect/Poll, and acceptance is the
// caller's job. The engine's real acceptance is durable and store-backed
// (Wave 2); scenario tests need only enough to prove duplicate delivery
// collapses to one accepted result, so this stays test-local rather than
// exported contract surface.
type acceptor struct {
	seen map[domain.InvocationID][]byte
}

func newAcceptor() *acceptor {
	return &acceptor{seen: make(map[domain.InvocationID][]byte)}
}

// accept reports whether the delivery was accepted: true on first delivery,
// false on a byte-identical replay. A divergent redelivery under the same id
// fails the test: that would be two results for one invocation, the §5.3
// impossibility.
func (a *acceptor) accept(t *testing.T, id domain.InvocationID, result any) bool {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("accept %s: marshal: %v", id, err)
	}
	prev, ok := a.seen[id]
	if !ok {
		a.seen[id] = b
		return true
	}
	if !bytes.Equal(prev, b) {
		t.Fatalf("conflicting redelivery for %s:\nfirst: %s\nlater: %s", id, prev, b)
	}
	return false
}
