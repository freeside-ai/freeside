package engine

import (
	"slices"
	"testing"
)

func TestProductionPublicationProgressEnum(t *testing.T) {
	t.Parallel()
	seen := map[productionPublicationProgress]bool{}
	for _, progress := range allProductionPublicationProgresses {
		if !progress.valid() {
			t.Errorf("progress %q is listed but not valid", progress)
		}
		if seen[progress] {
			t.Errorf("progress %q is listed twice", progress)
		}
		seen[progress] = true
	}
	if productionPublicationProgress("").valid() {
		t.Error("zero progress is valid")
	}
}

// TestProductionPublicationProgressOf pins all eight fact combinations. The
// overlapping rows pin the first-match order: a ready item without a
// checkpoint is refused before anything else, no checkpoint ignores the
// intent (it isn't read then), and a ready item wins over a finalized intent.
func TestProductionPublicationProgressOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                    string
		checkpointFound, readyExists, finalized bool
		want                                    productionPublicationProgress
		wantOK                                  bool
	}{
		{name: "nothing", want: productionPublicationProgressUnverified, wantOK: true},
		{
			name: "finalized intent without checkpoint", finalized: true,
			want: productionPublicationProgressUnverified, wantOK: true,
		},
		{name: "ready item without checkpoint", readyExists: true},
		{name: "ready item and finalized intent without checkpoint", readyExists: true, finalized: true},
		{
			name: "checkpoint", checkpointFound: true,
			want: productionPublicationProgressVerified, wantOK: true,
		},
		{
			name: "checkpoint and finalized intent", checkpointFound: true, finalized: true,
			want: productionPublicationProgressIntentFinalized, wantOK: true,
		},
		{
			name: "checkpoint and ready item", checkpointFound: true, readyExists: true,
			want: productionPublicationProgressReady, wantOK: true,
		},
		{
			name: "checkpoint, ready item, and finalized intent", checkpointFound: true, readyExists: true, finalized: true,
			want: productionPublicationProgressReady, wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := productionPublicationProgressOf(tc.checkpointFound, tc.readyExists, tc.finalized)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("productionPublicationProgressOf(%t, %t, %t) = %q, %t; want %q, %t",
					tc.checkpointFound, tc.readyExists, tc.finalized, got, ok, tc.want, tc.wantOK)
			}
			if ok && !slices.Contains(allProductionPublicationProgresses, got) {
				t.Errorf("progress %q is not in allProductionPublicationProgresses", got)
			}
		})
	}
}

func TestProductionPublicationProgressHasCheckpoint(t *testing.T) {
	t.Parallel()
	for _, progress := range allProductionPublicationProgresses {
		want := progress != productionPublicationProgressUnverified
		if got := progress.hasCheckpoint(); got != want {
			t.Errorf("%q.hasCheckpoint() = %t, want %t", progress, got, want)
		}
	}
	if productionPublicationProgress("").hasCheckpoint() {
		t.Error("zero progress has a checkpoint")
	}
}
