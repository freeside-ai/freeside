package signet

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestBoundedRunOutcomeTextCutsAtARuneBoundary(t *testing.T) {
	t.Parallel()
	short := strings.Repeat("a", maxRunOutcomeTextBytes)
	if got := boundedRunOutcomeText(short); got != short {
		t.Fatal("text at the cap was cut")
	}
	// A three-byte rune straddles the cut point.
	long := strings.Repeat("a", maxRunOutcomeTextBytes-len(runOutcomeTruncatedNote)-1) + strings.Repeat("€", 10)
	got := boundedRunOutcomeText(long)
	if len(got) > maxRunOutcomeTextBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, runOutcomeTruncatedNote) {
		t.Fatalf("cut text: %d bytes, valid %v, suffix %q", len(got), utf8.ValidString(got), got[len(got)-20:])
	}
}

func TestSetAsideFindingSaysAReversedFixWasReversed(t *testing.T) {
	t.Parallel()
	stored := domain.ReviewDispositionRecord{
		FindingID: "finding-1", Round: 1, Disposition: domain.ReviewDispositionFixed, Reason: "Bounded the loop.",
	}
	if _, ok := setAsideFinding(store.EffectiveFindingDisposition{
		Stored: stored, Effective: domain.ReviewDispositionFixed,
	}); ok {
		t.Fatal("a standing fix was listed")
	}
	got, ok := setAsideFinding(store.EffectiveFindingDisposition{
		Stored: stored, Effective: domain.ReviewDispositionDeclined,
		Supersession: &domain.FindingDispositionSupersession{FindingID: "finding-1", SupersededRound: 1, ReversingRound: 3},
	})
	want := RunOutcomeFinding{
		FindingID: "finding-1", Disposition: domain.ReviewDispositionDeclined, Round: 3,
		Reason: "Review round 3 found the round 1 fix reversed.",
	}
	if !ok || got != want {
		t.Fatalf("reversed fix = %+v, want %+v", got, want)
	}
	deferred := stored
	deferred.Disposition, deferred.Reason = domain.ReviewDispositionDeferred, "Tracked as a follow-up."
	got, ok = setAsideFinding(store.EffectiveFindingDisposition{Stored: deferred, Effective: deferred.Disposition})
	if !ok || got.Reason != deferred.Reason || got.Round != 1 || got.Disposition != domain.ReviewDispositionDeferred {
		t.Fatalf("deferral = %+v", got)
	}
}

func TestLatestExternalDispositionsKeepsTheHighestRound(t *testing.T) {
	t.Parallel()
	got := latestExternalDispositions([]domain.ExternalFindingDisposition{
		{FindingID: "b", Round: 2, Disposition: domain.ReviewDispositionDeferred},
		{FindingID: "a", Round: 3, Disposition: domain.ReviewDispositionFixed},
		{FindingID: "b", Round: 4, Disposition: domain.ReviewDispositionFixed},
		{FindingID: "a", Round: 2, Disposition: domain.ReviewDispositionDeclined},
	})
	if len(got) != 2 || got[0].FindingID != "a" || got[0].Round != 3 || got[1].FindingID != "b" || got[1].Round != 4 {
		t.Fatalf("latest = %+v", got)
	}
}

func TestRunOutcomePostStatesAreValid(t *testing.T) {
	t.Parallel()
	for _, state := range AllRunOutcomePostStates {
		if !state.valid() {
			t.Errorf("%q is listed but not valid", state)
		}
	}
	if RunOutcomePostState("").valid() {
		t.Error("the zero value is valid")
	}
}
