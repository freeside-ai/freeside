package keyedrun_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
)

// TestGroupRunsKeysApartAndSkipsABusyKey: a stalled key does not delay
// another key, a second run for the stalled key is skipped, and Wait joins
// the stalled run once it returns.
func TestGroupRunsKeysApartAndSkipsABusyKey(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var group keyedrun.Group[string]
		release := make(chan struct{})
		if group.Go("a", func() { <-release }) == nil {
			t.Fatal("first run for a key was skipped")
		}
		if group.Go("a", func() { t.Error("second run for a busy key ran") }) != nil {
			t.Fatal("busy key started a second run")
		}
		ran := false
		<-group.Go("b", func() { ran = true })
		if !ran || !group.Running("a") || group.Running("b") {
			t.Fatalf("ran = %v, a running = %v, b running = %v", ran, group.Running("a"), group.Running("b"))
		}
		close(release)
		group.Wait()
		if group.Running("a") {
			t.Fatal("key still running after Wait")
		}
		if group.Go("a", func() {}) == nil {
			t.Fatal("key was skipped after its run returned")
		}
		group.Wait()
	})
}

// TestPartitionKeepsFirstAppearanceOrder: parts come in the order each key
// first appears, and each part keeps its items' order.
func TestPartitionKeepsFirstAppearanceOrder(t *testing.T) {
	t.Parallel()
	parts := keyedrun.Partition([]string{"b1", "a1", "b2", "c1", "a2"}, func(s string) byte { return s[0] })
	var keys []byte
	var items [][]string
	for _, part := range parts {
		keys = append(keys, part.Key)
		items = append(items, part.Items)
	}
	if string(keys) != "bac" || !slices.EqualFunc(items, [][]string{{"b1", "b2"}, {"a1", "a2"}, {"c1"}}, slices.Equal) {
		t.Fatalf("keys = %q, items = %q", keys, items)
	}
}

// TestRunWaitsForItsOwnRunsAndJoinsErrors: Run returns once the runs it
// started return, joins their errors, and skips a key another run holds
// without waiting for it.
func TestRunWaitsForItsOwnRunsAndJoinsErrors(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var group keyedrun.Group[string]
		release := make(chan struct{})
		group.Go("busy", func() { <-release })
		failed := errors.New("b failed")
		var ran []string
		err := keyedrun.Run(&group, []keyedrun.Part[string, string]{
			{Key: "busy", Items: []string{"skipped"}},
			{Key: "b", Items: []string{"b1"}},
		}, func(items []string) error {
			ran = append(ran, items...)
			return failed
		})
		if !errors.Is(err, failed) || !slices.Equal(ran, []string{"b1"}) {
			t.Fatalf("err = %v, ran = %q", err, ran)
		}
		close(release)
		group.Wait()
	})
}
