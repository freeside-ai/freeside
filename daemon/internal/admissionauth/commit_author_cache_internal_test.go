package admissionauth

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestCommitAuthorCacheServesOtherInvocationWhileOneStalls: while one
// invocation's author check hangs on GitHub, another invocation's check runs
// and is cached, and the stalled check still caches when it ends.
func TestCommitAuthorCacheServesOtherInvocationWhileOneStalls(t *testing.T) {
	t.Parallel()
	cache := NewCommitAuthorCache()
	first := productionCommitAuthorAuthenticationBinding{repo: "example/a", appSlug: "app", botUserID: 1}
	second := productionCommitAuthorAuthenticationBinding{repo: "example/b", appSlug: "app", botUserID: 1}
	release, reached := make(chan struct{}), make(chan struct{})
	stalled := make(chan error, 1)
	go func() {
		stalled <- cache.authenticate("inv-a", first, func() error {
			close(reached)
			<-release
			return nil
		})
	}()
	<-reached
	done := make(chan error, 1)
	go func() { done <- cache.authenticate("inv-b", second, func() error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("second invocation waited on the first invocation's check")
	}
	close(release)
	if err := <-stalled; err != nil {
		t.Fatal(err)
	}
	for id, binding := range map[domain.InvocationID]productionCommitAuthorAuthenticationBinding{"inv-a": first, "inv-b": second} {
		if err := cache.authenticate(id, binding, func() error { return errors.New("authenticated again") }); err != nil {
			t.Fatalf("%s was not cached: %v", id, err)
		}
	}
}
