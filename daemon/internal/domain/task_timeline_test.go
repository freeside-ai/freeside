package domain

import (
	"errors"
	"strings"
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
	externalReview := TaskEventExternalReview{ItemID: "ready", FindingID: "finding", ReviewerLogin: "octocat", Round: 2, HeadSHA: "head"}
	line := TaskEventTaskLine{
		LineID: Digest("sha256:" + strings.Repeat("a", 64)), Role: RoleImplementer, Agent: ptr("claude-b"), Version: 1,
		Source: TaskLineSourceRouter,
		Provenance: &TaskLineProvenance{
			PolicyRevision: 1, Trigger: TaskLineSelectionExhaustedMark,
			Reading:    &TaskLineSelectionReading{Pool: "claude-a", Window: "seven_day_opus", ObservedAt: at},
			PassedOver: ptr("claude-a"),
		},
	}
	fixtures := []TaskEvent{
		{Kind: TaskEventCreated, RecordedAt: at},
		{Kind: TaskEventCampaignAllocated, RecordedAt: at, CampaignID: ptr(CampaignID("campaign-1")), SpecificationRunID: ptr(RunID("spec-1"))},
		{Kind: TaskEventSpecificationApproved, RecordedAt: at, CampaignID: ptr(CampaignID("campaign-1")), RunID: ptr(RunID("impl-1")), ApprovedSpecDigest: ptr(Digest("sha256:spec")), SpecificationRunID: ptr(RunID("spec-1"))},
		{Kind: TaskEventPROpened, RecordedAt: at, RunID: ptr(RunID("impl-1")), PRNumber: ptr(42)},
		{Kind: TaskEventPRMerged, RecordedAt: at, RunID: ptr(RunID("impl-1")), PRNumber: ptr(42), MergeCommitSHA: ptr("abc123")},
		{Kind: TaskEventExternalReviewReentered, RecordedAt: at, RunID: ptr(RunID("impl-1")), PRNumber: ptr(42), ExternalReview: &externalReview},
		{Kind: TaskEventTaskLineSet, RecordedAt: at, Line: &line},
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
				func(e *TaskEvent) { e.ExternalReview = toggled(e.ExternalReview, externalReview) },
				func(e *TaskEvent) { e.Line = toggled(e.Line, line) },
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
	// Every external review detail is required, and the login is the
	// forge's: one a trust profile could not list is not one.
	for name, change := range map[string]func(*TaskEventExternalReview){
		"no item":           func(x *TaskEventExternalReview) { x.ItemID = "" },
		"no finding":        func(x *TaskEventExternalReview) { x.FindingID = "" },
		"no login":          func(x *TaskEventExternalReview) { x.ReviewerLogin = "" },
		"spaced login":      func(x *TaskEventExternalReview) { x.ReviewerLogin = "octo cat" },
		"no round":          func(x *TaskEventExternalReview) { x.Round = 0 },
		"no published head": func(x *TaskEventExternalReview) { x.HeadSHA = "" },
	} {
		detail := externalReview
		change(&detail)
		invalid := TaskEvent{Kind: TaskEventExternalReviewReentered, RecordedAt: at, RunID: ptr(RunID("run-1")), PRNumber: ptr(42), ExternalReview: &detail}
		if err := invalid.Validate(); !errors.Is(err, ErrTaskEventDetailMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A line detail is the line version's own shape: an operator line has no
	// provenance, a router line has one that validates, and only a later
	// version names the agent it superseded.
	operator := TaskEventTaskLine{
		LineID: line.LineID, Role: RoleReviewer, Agent: ptr("codex"), Version: 2,
		Source: TaskLineSourceSubmitTask, SupersededAgent: ptr("claude-a"),
	}
	// A cleared version names no agent, and the version after it supersedes
	// none.
	cleared := TaskEventTaskLine{
		LineID: line.LineID, Role: RoleReviewer, Version: 3,
		Source: TaskLineSourceCLILineChange, SupersededAgent: ptr("codex"),
	}
	afterClear := TaskEventTaskLine{
		LineID: line.LineID, Role: RoleReviewer, Agent: ptr("codex"), Version: 4,
		Source: TaskLineSourceCLILineChange,
	}
	for name, detail := range map[string]TaskEventTaskLine{"operator": operator, "cleared": cleared, "after clear": afterClear} {
		if err := (TaskEvent{Kind: TaskEventTaskLineSet, RecordedAt: at, Line: &detail}).Validate(); err != nil {
			t.Fatalf("%s line event: %v", name, err)
		}
	}
	for name, change := range map[string]func(*TaskEventTaskLine){
		"bad line id":                func(l *TaskEventTaskLine) { l.LineID = "line-1" },
		"ineligible role":            func(l *TaskEventTaskLine) { l.Role = RoleShadowReviewer },
		"bad agent":                  func(l *TaskEventTaskLine) { l.Agent = ptr("Bad Agent") },
		"zero version":               func(l *TaskEventTaskLine) { l.Version = 0 },
		"first that supersedes":      func(l *TaskEventTaskLine) { l.SupersededAgent = ptr("codex") },
		"bad superseded agent":       func(l *TaskEventTaskLine) { l.Version = 2; l.SupersededAgent = ptr("Bad Agent") },
		"unknown source":             func(l *TaskEventTaskLine) { l.Source = "intake" },
		"router without provenance":  func(l *TaskEventTaskLine) { l.Provenance = nil },
		"operator with provenance":   func(l *TaskEventTaskLine) { l.Source = TaskLineSourceCLISubmit },
		"router line names no agent": func(l *TaskEventTaskLine) { l.Agent = nil },
		"cleared version 1": func(l *TaskEventTaskLine) {
			l.Agent, l.Provenance, l.Source = nil, nil, TaskLineSourceCLILineChange
		},
		"cleared by a submission": func(l *TaskEventTaskLine) {
			l.Agent, l.Provenance, l.Version, l.Source = nil, nil, 2, TaskLineSourceCLISubmit
		},
		"invalid provenance": func(l *TaskEventTaskLine) {
			l.Provenance = &TaskLineProvenance{PolicyRevision: 1, Trigger: TaskLineSelectionFirstAdmission, PassedOver: ptr("claude-a")}
		},
	} {
		detail := line
		change(&detail)
		invalid := TaskEvent{Kind: TaskEventTaskLineSet, RecordedAt: at, Line: &detail}
		if err := invalid.Validate(); !errors.Is(err, ErrTaskEventDetailMismatch) {
			t.Errorf("%s: %v", name, err)
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
	if len(fixtures)+7 != len(AllTaskEventKinds) {
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
			invalid = e
			invalid.ExternalReview = toggled(invalid.ExternalReview, TaskEventExternalReview{})
			if err := invalid.Validate(); err == nil {
				t.Fatal("unrelated external review accepted")
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
