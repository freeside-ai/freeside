package domain

import (
	"errors"
	"testing"
	"time"
)

func TestTaskTimelineEnums(t *testing.T) {
	for _, kind := range AllTaskEventKinds {
		if !kind.valid() {
			t.Errorf("registered event %q is invalid", kind)
		}
	}
	for _, role := range AllTaskRunRoles {
		if !role.valid() {
			t.Errorf("registered role %q is invalid", role)
		}
	}
	if TaskEventKind("").valid() || TaskEventKind("named").valid() ||
		TaskRunRole("").valid() || TaskRunRole("review").valid() {
		t.Fatal("unregistered timeline vocabulary validates")
	}
}

func TestTaskEventDetailContract(t *testing.T) {
	at := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	fixtures := []TaskEvent{
		{Kind: TaskEventCreated, RecordedAt: at},
		{Kind: TaskEventCampaignAllocated, RecordedAt: at, CampaignID: ptr(CampaignID("campaign-1")), SpecificationRunID: ptr(RunID("spec-1"))},
		{Kind: TaskEventSpecificationApproved, RecordedAt: at, CampaignID: ptr(CampaignID("campaign-1")), RunID: ptr(RunID("impl-1")), ApprovedSpecDigest: ptr(Digest("sha256:spec")), SpecificationRunID: ptr(RunID("spec-1"))},
		{Kind: TaskEventPROpened, RecordedAt: at, RunID: ptr(RunID("impl-1")), PRNumber: ptr(42)},
		{Kind: TaskEventPRMerged, RecordedAt: at, RunID: ptr(RunID("impl-1")), PRNumber: ptr(42), MergeCommitSHA: ptr("abc123")},
	}
	for _, event := range fixtures {
		t.Run(string(event.Kind), func(t *testing.T) {
			if err := event.Validate(); err != nil {
				t.Fatal(err)
			}
			// Each field belongs to exactly its declared union arms. Removing a
			// required field or adding a forbidden one must reject the event.
			for _, change := range []func(*TaskEvent){
				func(e *TaskEvent) { e.CampaignID = toggled(e.CampaignID, CampaignID("campaign-1")) },
				func(e *TaskEvent) { e.RunID = toggled(e.RunID, RunID("impl-1")) },
				func(e *TaskEvent) { e.ApprovedSpecDigest = toggled(e.ApprovedSpecDigest, Digest("sha256:spec")) },
				func(e *TaskEvent) { e.SpecificationRunID = toggled(e.SpecificationRunID, RunID("spec-1")) },
				func(e *TaskEvent) { e.PRNumber = toggled(e.PRNumber, 42) },
				func(e *TaskEvent) { e.MergeCommitSHA = toggled(e.MergeCommitSHA, "abc123") },
			} {
				invalid := event
				change(&invalid)
				if err := invalid.Validate(); !errors.Is(err, ErrTaskEventDetailMismatch) {
					t.Errorf("changed event %+v: %v", invalid, err)
				}
			}
		})
	}
	for _, invalid := range []TaskEvent{
		{Kind: "named", RecordedAt: at},
		{Kind: TaskEventCreated},
		{Kind: TaskEventCreated, RecordedAt: at.In(time.FixedZone("local", 3600))},
		{Kind: TaskEventPROpened, RecordedAt: at, RunID: ptr(RunID("")), PRNumber: ptr(42)},
		{Kind: TaskEventPROpened, RecordedAt: at, RunID: ptr(RunID("run-1")), PRNumber: ptr(0)},
		{Kind: TaskEventPRMerged, RecordedAt: at, RunID: ptr(RunID("run-1")), PRNumber: ptr(42), MergeCommitSHA: ptr("")},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid event %+v accepted", invalid)
		}
	}
}

func toggled[T any](value *T, replacement T) *T {
	if value != nil {
		return nil
	}
	return &replacement
}

func TestTaskHistoryEventDetails(t *testing.T) {
	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	run := RunID("run")
	review := TaskEventReview{InvocationID: "review", Round: 2, HeadSHA: "head", BaseSHA: "base"}
	completed := review
	completed.Outcome = ptr(ReviewClean)
	failed := review
	failed.Failure = ptr(AllReviewFailureClasses[0])
	fixtures := []TaskEvent{
		{Kind: TaskEventStarted, RecordedAt: at, RunID: &run},
		{Kind: TaskEventCompleted, RecordedAt: at, RunID: &run},
		{Kind: TaskEventAbandoned, RecordedAt: at, RunID: &run},
		{Kind: TaskEventStopRequested, RecordedAt: at},
		{Kind: TaskEventStopped, RecordedAt: at},
		{Kind: TaskEventStopFailed, RecordedAt: at},
		{Kind: TaskEventRunMilestone, RecordedAt: at, RunID: &run, Milestone: &RunMilestone{RunID: run, Kind: MilestoneRunSubmitted, InvocationID: ptr(InvocationID("inv")), RecordedAt: at}},
		{Kind: TaskEventReviewRequested, RecordedAt: at, RunID: &run, Review: &review},
		{Kind: TaskEventReviewCompleted, RecordedAt: at, RunID: &run, Review: &completed},
		{Kind: TaskEventReviewFailed, RecordedAt: at, RunID: &run, Review: &failed},
		{Kind: TaskEventVerificationRecorded, RecordedAt: at, RunID: &run, Verification: &TaskEventVerification{ItemID: "ready", Class: ReadinessReadyClean, HeadSHA: "head", BaseSHA: "base"}},
	}
	if len(fixtures)+5 != len(AllTaskEventKinds) {
		t.Fatal("new event kinds need detail fixtures")
	}
	for _, e := range fixtures {
		t.Run(string(e.Kind), func(t *testing.T) {
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			invalid := e
			invalid.PRNumber = ptr(1)
			if err := invalid.Validate(); err == nil {
				t.Fatal("unrelated PR detail accepted")
			}
			invalid = e
			invalid.Milestone = toggled(invalid.Milestone, RunMilestone{})
			if err := invalid.Validate(); err == nil {
				t.Fatal("missing or unrelated milestone accepted")
			}
			invalid = e
			invalid.Review = toggled(invalid.Review, TaskEventReview{})
			if err := invalid.Validate(); err == nil {
				t.Fatal("missing or unrelated review accepted")
			}
			invalid = e
			invalid.Verification = toggled(invalid.Verification, TaskEventVerification{})
			if err := invalid.Validate(); err == nil {
				t.Fatal("missing or unrelated verification accepted")
			}
		})
	}
	invalid := fixtures[6]
	copy := *invalid.Milestone
	copy.RunID = "other"
	invalid.Milestone = &copy
	if err := invalid.Validate(); err == nil {
		t.Fatal("cross-run milestone accepted")
	}
	invalid = fixtures[7]
	invalid.Review = &completed
	if err := invalid.Validate(); err == nil {
		t.Fatal("request promoted to a clean result")
	}
	invalid = fixtures[10]
	v := *invalid.Verification
	v.Class = ReadinessBlocked
	invalid.Verification = &v
	if err := invalid.Validate(); err == nil {
		t.Fatal("blocked verdict laundered as recorded readiness")
	}
}
