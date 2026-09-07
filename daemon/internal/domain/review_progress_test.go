package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestReviewRequestRecord(t *testing.T) {
	r := ReviewRequestRecord{
		InvocationID: "review-1", RunID: "run-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", RequestedAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "review-request", body)
	for name, mutate := range map[string]func(*ReviewRequestRecord){
		"invocation": func(r *ReviewRequestRecord) { r.InvocationID = "" },
		"run":        func(r *ReviewRequestRecord) { r.RunID = "" },
		"round":      func(r *ReviewRequestRecord) { r.Round = 0 },
		"base":       func(r *ReviewRequestRecord) { r.BaseSHA = "" },
		"head":       func(r *ReviewRequestRecord) { r.HeadSHA = "" },
		"time":       func(r *ReviewRequestRecord) { r.RequestedAt = time.Time{} },
		"UTC":        func(r *ReviewRequestRecord) { r.RequestedAt = r.RequestedAt.In(time.FixedZone("local", 3600)) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := r
			mutate(&bad)
			if bad.Validate() == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	for _, state := range AllReviewProgressStates {
		if !state.valid() {
			t.Fatal(state)
		}
	}
	for _, source := range AllReviewSourceKinds {
		if !source.valid() {
			t.Fatal(source)
		}
	}
	for _, availability := range AllReviewEvidenceAvailabilities {
		if !availability.valid() {
			t.Fatal(availability)
		}
	}
	for _, kind := range AllReviewContentKinds {
		if !kind.valid() {
			t.Fatal(kind)
		}
	}
	if ReviewContentKind("").valid() || ReviewContentKind("other").valid() {
		t.Fatal("invalid content kind")
	}
	if ReviewProgressState("").valid() || ReviewSourceKind("").valid() || ReviewEvidenceAvailability("").valid() || ReviewEvidenceAvailability("other").valid() {
		t.Fatal("invalid zero value")
	}
}
