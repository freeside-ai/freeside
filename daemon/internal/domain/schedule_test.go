package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func ptr[T any](v T) *T { return &v }

// validScheduleInput builds a valid input for each kind; the per-kind detail
// contract is what Validate enforces, so these double as its positive cases.
func validScheduleInput(kind ScheduleKind) ScheduleInput {
	ts := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	runID := RunID("run-1")
	policyDigest := Digest("sha256:policy")
	in := ScheduleInput{
		ID:        ScheduleID("schedule-" + string(kind) + "-x"),
		ProjectID: "project-1",
		Kind:      kind,
		CreatedAt: ts,
	}
	switch kind {
	case SchedulePRChecksDeadline, ScheduleReviewWaitThreshold:
		in.RunID, in.PolicyDigest = &runID, &policyDigest
		in.Subject = ScheduleSubject{
			Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")), ItemVersion: ptr(1),
		}
		in.FireAt = ptr(ts.Add(30 * time.Minute))
	case ScheduleBaseAdvanceWatch:
		in.RunID, in.PolicyDigest = &runID, &policyDigest
		in.Subject = ScheduleSubject{
			Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")), ItemVersion: ptr(1),
		}
		in.IntervalSeconds = ptr(int64(900))
		in.BaseWatch = &ScheduleBaseWatch{
			Repo: "owner/repo", BaseRef: "main", AdmittedBaseSHA: "cafebabe",
		}
	case ScheduleInstallationPoll:
		in.Subject = ScheduleSubject{
			Type:           ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(4385298)), ActiveEpoch: ptr(int64(1)),
			DurableIntentRevision: ptr(int64(2)),
		}
		in.IntervalSeconds = ptr(int64(2))
		in.ExpiresAt = ptr(ts.Add(10 * time.Minute))
	case ScheduleDoctor, ScheduleJanitor:
		in.Subject = ScheduleSubject{Type: ScheduleSubjectTrustedConfig}
		in.IntervalSeconds = ptr(int64(30))
	}
	return in
}

func mustSchedule(t *testing.T, kind ScheduleKind) Schedule {
	t.Helper()
	s, err := NewSchedule(validScheduleInput(kind))
	if err != nil {
		t.Fatalf("NewSchedule(%s): %v", kind, err)
	}
	return s
}

func TestNewScheduleEveryKind(t *testing.T) {
	for _, kind := range AllScheduleKinds {
		s := mustSchedule(t, kind)
		if s.Generation != 1 || s.Status != ScheduleArmed || s.Resolution != nil {
			t.Fatalf("NewSchedule(%s) = gen %d status %s", kind, s.Generation, s.Status)
		}
	}
}

func TestScheduleKindContract(t *testing.T) {
	// The classification methods are dispatch switches; this pins each 1B
	// member's classification so a new kind must both compile (exhaustive
	// linter) and declare itself here.
	oneShot := map[ScheduleKind]bool{
		SchedulePRChecksDeadline: true, ScheduleReviewWaitThreshold: true,
	}
	trusted := map[ScheduleKind]bool{ScheduleDoctor: true, ScheduleJanitor: true}
	for _, kind := range AllScheduleKinds {
		if kind.OneShot() != oneShot[kind] {
			t.Errorf("%s OneShot() = %v", kind, kind.OneShot())
		}
		if kind.TrustedConfigJob() != trusted[kind] {
			t.Errorf("%s TrustedConfigJob() = %v", kind, kind.TrustedConfigJob())
		}
		if kind.subjectType() == "" || !kind.subjectType().valid() {
			t.Errorf("%s subjectType() = %q", kind, kind.subjectType())
		}
	}
	if ScheduleKind("").OneShot() || ScheduleKind("").TrustedConfigJob() {
		t.Error("zero kind classifies")
	}
	if ScheduleKind("").subjectType() != "" {
		t.Error("zero kind binds a subject type")
	}
}

// TestScheduleModeEligibilityMatrix pins the §5.16 per-mode eligibility of
// every kind as a golden matrix: a consumer that narrows a kind's modes
// changes EligibleIn, this matrix, and the work unit's decision note
// together.
func TestScheduleModeEligibilityMatrix(t *testing.T) {
	type row struct {
		Kind     ScheduleKind           `json:"kind"`
		Eligible map[OperatingMode]bool `json:"eligible"`
	}
	rows := make([]row, 0, len(AllScheduleKinds))
	for _, kind := range AllScheduleKinds {
		r := row{Kind: kind, Eligible: map[OperatingMode]bool{}}
		for _, mode := range AllOperatingModes {
			r.Eligible[mode] = kind.EligibleIn(mode)
		}
		rows = append(rows, r)
		if kind.EligibleIn(OperatingMode("")) {
			t.Errorf("%s eligible under invalid mode", kind)
		}
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "schedule_mode_eligibility", append(got, '\n'))
}

func TestScheduleValidateDetailContract(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Schedule)
		wantErr error
	}{
		{"empty id", func(s *Schedule) { s.ID = "" }, ErrEmptyID},
		{"empty project", func(s *Schedule) { s.ProjectID = "" }, ErrEmptyID},
		{"invalid kind", func(s *Schedule) { s.Kind = "cron" }, ErrInvalidScheduleKind},
		{"zero generation", func(s *Schedule) { s.Generation = 0 }, ErrNonPositive},
		{"one-shot without fire_at", func(s *Schedule) { s.FireAt = nil }, ErrScheduleDetailMismatch},
		{"one-shot with interval", func(s *Schedule) { s.IntervalSeconds = ptr(int64(60)) }, ErrScheduleDetailMismatch},
		{"one-shot with base watch", func(s *Schedule) {
			s.BaseWatch = &ScheduleBaseWatch{Repo: "o/r", BaseRef: "main", AdmittedBaseSHA: "sha"}
		}, ErrScheduleDetailMismatch},
		{"wrong subject class", func(s *Schedule) {
			s.Subject = ScheduleSubject{Type: ScheduleSubjectTrustedConfig}
		}, ErrScheduleDetailMismatch},
		{"workload without run", func(s *Schedule) { s.RunID = nil }, ErrScheduleDetailMismatch},
		{"workload without policy", func(s *Schedule) { s.PolicyDigest = nil }, ErrScheduleDetailMismatch},
		{"workload with empty run", func(s *Schedule) { s.RunID = ptr(RunID("")) }, ErrEmptyID},
		{"workload with empty policy", func(s *Schedule) { s.PolicyDigest = ptr(Digest("")) }, ErrEmptyField},
		{"non-UTC fire_at", func(s *Schedule) {
			loc := time.FixedZone("x", 3600)
			s.FireAt = ptr(s.FireAt.In(loc))
		}, ErrTimestampNotUTC},
		{"armed with resolution", func(s *Schedule) {
			s.Resolution = &ScheduleResolution{Reason: ResolutionDeadlineElapsed, RecordedAt: s.CreatedAt}
		}, ErrScheduleDetailMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustSchedule(t, SchedulePRChecksDeadline)
			tc.mutate(&s)
			if err := s.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}

	t.Run("recurring detail", func(t *testing.T) {
		s := mustSchedule(t, ScheduleBaseAdvanceWatch)
		s.IntervalSeconds = nil
		if err := s.Validate(); !errors.Is(err, ErrScheduleDetailMismatch) {
			t.Fatalf("recurring without interval: %v", err)
		}
		s = mustSchedule(t, ScheduleBaseAdvanceWatch)
		s.IntervalSeconds = ptr(int64(0))
		if err := s.Validate(); !errors.Is(err, ErrNonPositive) {
			t.Fatalf("zero interval: %v", err)
		}
		s = mustSchedule(t, ScheduleBaseAdvanceWatch)
		s.BaseWatch = nil
		if err := s.Validate(); !errors.Is(err, ErrScheduleDetailMismatch) {
			t.Fatalf("base watch without detail: %v", err)
		}
		s = mustSchedule(t, ScheduleBaseAdvanceWatch)
		s.BaseWatch.AdmittedBaseSHA = ""
		if err := s.Validate(); !errors.Is(err, ErrEmptyField) {
			t.Fatalf("empty admitted base: %v", err)
		}
	})

	t.Run("non-workload authority", func(t *testing.T) {
		for _, kind := range []ScheduleKind{ScheduleInstallationPoll, ScheduleDoctor, ScheduleJanitor} {
			s := mustSchedule(t, kind)
			s.RunID = ptr(RunID("run-1"))
			s.PolicyDigest = ptr(Digest("sha256:policy"))
			if err := s.Validate(); !errors.Is(err, ErrScheduleDetailMismatch) {
				t.Errorf("%s with run authority: %v", kind, err)
			}
		}
	})

	t.Run("expiry contract", func(t *testing.T) {
		for _, kind := range []ScheduleKind{SchedulePRChecksDeadline, ScheduleReviewWaitThreshold} {
			in := validScheduleInput(kind)
			in.ExpiresAt = ptr(in.CreatedAt.Add(time.Hour))
			if _, err := NewSchedule(in); !errors.Is(err, ErrScheduleDetailMismatch) {
				t.Errorf("NewSchedule(%s) with expiry: %v", kind, err)
			}
			s := mustSchedule(t, kind)
			s.ExpiresAt = ptr(s.CreatedAt.Add(time.Hour))
			if err := s.Validate(); !errors.Is(err, ErrScheduleDetailMismatch) {
				t.Errorf("%s with expiry: %v", kind, err)
			}
			s = mustSchedule(t, kind)
			if _, err := s.Concluded(
				ScheduleExpired, ResolutionIntentExpired, s.CreatedAt.Add(time.Hour),
			); !errors.Is(err, ErrInvalidScheduleStatus) {
				t.Errorf("%s concluded expired: %v", kind, err)
			}
		}
		s := mustSchedule(t, ScheduleInstallationPoll)
		s.ExpiresAt = nil
		if err := s.Validate(); !errors.Is(err, ErrScheduleDetailMismatch) {
			t.Fatalf("installation poll without expiry: %v", err)
		}
		s = mustSchedule(t, ScheduleJanitor)
		s.ExpiresAt = ptr(s.CreatedAt.Add(time.Hour))
		if err := s.Validate(); !errors.Is(err, ErrScheduleDetailMismatch) {
			t.Fatalf("permanent job with expiry: %v", err)
		}
	})
}

func TestScheduleSubjectValidate(t *testing.T) {
	valid := ScheduleSubject{
		Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")), ItemVersion: ptr(1),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		subject ScheduleSubject
		wantErr error
	}{
		{"zero type", ScheduleSubject{}, ErrInvalidScheduleSubjectType},
		{"item without version", ScheduleSubject{
			Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")),
		}, ErrScheduleDetailMismatch},
		{"item with intent fields", ScheduleSubject{
			Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")),
			ItemVersion: ptr(1), RegistrationID: ptr(int64(1)),
		}, ErrScheduleDetailMismatch},
		{"empty item id", ScheduleSubject{
			Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("")), ItemVersion: ptr(1),
		}, ErrEmptyID},
		{"non-positive item version", ScheduleSubject{
			Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")), ItemVersion: ptr(0),
		}, ErrNonPositive},
		{"intent missing epoch", ScheduleSubject{
			Type:           ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(1)), DurableIntentRevision: ptr(int64(1)),
		}, ErrScheduleDetailMismatch},
		{"intent non-positive revision", ScheduleSubject{
			Type:           ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(1)), ActiveEpoch: ptr(int64(1)),
			DurableIntentRevision: ptr(int64(0)),
		}, ErrNonPositive},
		{"trusted config with item", ScheduleSubject{
			Type: ScheduleSubjectTrustedConfig, ItemID: ptr(ItemID("item-1")), ItemVersion: ptr(1),
		}, ErrScheduleDetailMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.subject.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestScheduleConcluded(t *testing.T) {
	ts := time.Date(2026, 2, 3, 5, 0, 0, 0, time.UTC)
	s := mustSchedule(t, SchedulePRChecksDeadline)

	fired, err := s.Concluded(ScheduleFired, ResolutionDeadlineElapsed, ts)
	if err != nil {
		t.Fatal(err)
	}
	if !fired.Status.Terminal() || fired.Resolution == nil || fired.Resolution.RecordedAt != ts {
		t.Fatalf("fired = %+v", fired)
	}
	if _, err := fired.Concluded(ScheduleResolved, ResolutionSubjectConcluded, ts); !errors.Is(err, ErrImmutableTransition) {
		t.Fatalf("re-concluding terminal: %v", err)
	}
	// Reason/status pairing is closed.
	if _, err := s.Concluded(ScheduleFired, ResolutionIntentExpired, ts); !errors.Is(err, ErrInvalidScheduleResolution) {
		t.Fatalf("fired with expiry reason: %v", err)
	}
	if _, err := s.Concluded(ScheduleExpired, ResolutionDeadlineElapsed, ts); !errors.Is(err, ErrInvalidScheduleResolution) {
		t.Fatalf("expired with deadline reason: %v", err)
	}
	// A recurring kind cannot terminate fired; a permanent job cannot
	// terminate at all.
	watch := mustSchedule(t, ScheduleBaseAdvanceWatch)
	if _, err := watch.Concluded(ScheduleFired, ResolutionDeadlineElapsed, ts); !errors.Is(err, ErrInvalidScheduleStatus) {
		t.Fatalf("recurring fired: %v", err)
	}
	if _, err := watch.Concluded(ScheduleResolved, ResolutionSubjectConcluded, ts); err != nil {
		t.Fatalf("watch resolved: %v", err)
	}
	janitor := mustSchedule(t, ScheduleJanitor)
	if _, err := janitor.Concluded(ScheduleResolved, ResolutionSubjectConcluded, ts); !errors.Is(err, ErrInvalidScheduleStatus) {
		t.Fatalf("permanent job concluded: %v", err)
	}
}

func TestScheduleReArmed(t *testing.T) {
	ts := time.Date(2026, 2, 3, 5, 0, 0, 0, time.UTC)
	s := mustSchedule(t, SchedulePRChecksDeadline)
	subject := ScheduleSubject{
		Type: ScheduleSubjectAttentionItem, ItemID: ptr(ItemID("item-1")), ItemVersion: ptr(3),
	}
	re, err := s.ReArmed(subject, ptr(ts.Add(time.Hour)), ts)
	if err != nil {
		t.Fatal(err)
	}
	if re.Generation != s.Generation+1 || re.Status != ScheduleArmed || re.Resolution != nil {
		t.Fatalf("re-armed = %+v", re)
	}
	if *re.Subject.ItemVersion != 3 {
		t.Fatalf("re-armed subject version = %d", *re.Subject.ItemVersion)
	}
	// Terminal schedules re-arm too: that is the recorded stale-event path.
	fired, err := s.Concluded(ScheduleFired, ResolutionDeadlineElapsed, ts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fired.ReArmed(subject, ptr(ts.Add(time.Hour)), ts); err != nil {
		t.Fatalf("re-arming terminal: %v", err)
	}
}

func TestValidateScheduleTransition(t *testing.T) {
	ts := time.Date(2026, 2, 3, 5, 0, 0, 0, time.UTC)
	armed := mustSchedule(t, SchedulePRChecksDeadline)
	fired, err := armed.Concluded(ScheduleFired, ResolutionDeadlineElapsed, ts)
	if err != nil {
		t.Fatal(err)
	}
	subject := *ptr(armed.Subject)
	reArmed, err := fired.ReArmed(subject, ptr(ts.Add(time.Hour)), ts)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		old     Schedule
		new     Schedule
		wantErr error
	}{
		{"idempotent replay", armed, armed, nil},
		{"conclude armed", armed, fired, nil},
		{"re-arm terminal", fired, reArmed, nil},
		{"terminal rewrite", fired, func() Schedule {
			s := fired
			s.Resolution = &ScheduleResolution{Reason: ResolutionDeadlineElapsed, RecordedAt: ts.Add(time.Hour)}
			return s
		}(), ErrImmutableTransition},
		{"same-generation drift", armed, func() Schedule {
			s := armed
			s.FireAt = ptr(ts.Add(2 * time.Hour))
			return s
		}(), ErrStaleTransition},
		{"conclusion rewriting armed fields", armed, func() Schedule {
			s := fired
			s.FireAt = ptr(ts.Add(2 * time.Hour))
			return s
		}(), ErrImmutableTransition},
		{"generation skip", armed, func() Schedule {
			s := reArmed
			s.Generation = armed.Generation + 2
			return s
		}(), ErrStaleTransition},
		{"generation regression", reArmed, armed, ErrStaleTransition},
		{"re-arm not armed", armed, func() Schedule {
			s := fired
			s.Generation = armed.Generation + 1
			return s
		}(), ErrStaleTransition},
		{"identity change", armed, func() Schedule {
			s := armed
			s.ID = "schedule-other"
			return s
		}(), ErrImmutableTransition},
		{"run binding change", armed, func() Schedule {
			s := armed
			s.RunID = ptr(RunID("run-2"))
			return s
		}(), ErrImmutableTransition},
		{"policy binding change", armed, func() Schedule {
			s := armed
			s.PolicyDigest = ptr(Digest("sha256:other"))
			return s
		}(), ErrImmutableTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScheduleTransition(tc.old, tc.new)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("transition: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("transition = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func validOccurrence() ScheduleOccurrence {
	ts := time.Date(2026, 2, 3, 4, 35, 6, 0, time.UTC)
	return ScheduleOccurrence{
		ScheduleID: "schedule-pr_checks_deadline-x", Generation: 1,
		NominalFireAt: ts, Status: OccurrencePending, CreatedAt: ts,
	}
}

func TestScheduleOccurrenceValidate(t *testing.T) {
	occ := validOccurrence()
	if err := occ.Validate(); err != nil {
		t.Fatal(err)
	}
	consumed := occ
	consumed.Status = OccurrenceConsumed
	consumed.ConsumedAt = ptr(occ.CreatedAt.Add(time.Second))
	consumed.Outcome = ptr(OutcomeHandled)
	if err := consumed.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		mutate  func(*ScheduleOccurrence)
		wantErr error
	}{
		{"empty schedule id", func(o *ScheduleOccurrence) { o.ScheduleID = "" }, ErrEmptyID},
		{"zero generation", func(o *ScheduleOccurrence) { o.Generation = 0 }, ErrNonPositive},
		{"pending with outcome", func(o *ScheduleOccurrence) { o.Outcome = ptr(OutcomeHandled) }, ErrScheduleDetailMismatch},
		{"pending with consumed_at", func(o *ScheduleOccurrence) { o.ConsumedAt = ptr(o.CreatedAt) }, ErrScheduleDetailMismatch},
		{"consumed without outcome", func(o *ScheduleOccurrence) {
			o.Status = OccurrenceConsumed
			o.ConsumedAt = ptr(o.CreatedAt)
		}, ErrScheduleDetailMismatch},
		{"gap not backwards", func(o *ScheduleOccurrence) {
			o.Gap = &ScheduleFireGap{MissedOccurrences: 1, EarliestMissedAt: o.NominalFireAt.Add(time.Second)}
		}, ErrTimestampOutOfOrder},
		{"gap zero misses", func(o *ScheduleOccurrence) {
			o.Gap = &ScheduleFireGap{MissedOccurrences: 0, EarliestMissedAt: o.NominalFireAt.Add(-time.Hour)}
		}, ErrNonPositive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := validOccurrence()
			tc.mutate(&o)
			if err := o.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewScheduleEvent(t *testing.T) {
	firedAt := time.Date(2026, 2, 3, 4, 40, 0, 0, time.UTC)
	s := mustSchedule(t, SchedulePRChecksDeadline)
	occ := validOccurrence()
	occ.ScheduleID = s.ID

	ev, err := NewScheduleEvent(s, occ, firedAt)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Generation != s.Generation || ev.Subject.Type != ScheduleSubjectAttentionItem ||
		*ev.Subject.ItemVersion != *s.Subject.ItemVersion {
		t.Fatalf("event = %+v", ev)
	}

	// The constructor is the trust boundary: a terminal schedule, a foreign
	// or stale-generation occurrence, and a consumed occurrence are all
	// unrepresentable as events.
	fired, err := s.Concluded(ScheduleFired, ResolutionDeadlineElapsed, firedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewScheduleEvent(fired, occ, firedAt); !errors.Is(err, ErrInvalidScheduleStatus) {
		t.Fatalf("terminal schedule: %v", err)
	}
	foreign := occ
	foreign.ScheduleID = "schedule-other"
	if _, err := NewScheduleEvent(s, foreign, firedAt); !errors.Is(err, ErrParentKeyMismatch) {
		t.Fatalf("foreign occurrence: %v", err)
	}
	stale := occ
	stale.Generation = s.Generation + 1
	if _, err := NewScheduleEvent(s, stale, firedAt); !errors.Is(err, ErrParentKeyMismatch) {
		t.Fatalf("stale generation: %v", err)
	}
	consumed := occ
	consumed.Status = OccurrenceConsumed
	consumed.ConsumedAt = ptr(firedAt)
	consumed.Outcome = ptr(OutcomeHandled)
	if _, err := NewScheduleEvent(s, consumed, firedAt); !errors.Is(err, ErrInvalidScheduleOccurrenceStatus) {
		t.Fatalf("consumed occurrence: %v", err)
	}
}

// TestScheduleEventCarriesNoAuthority pins the event's exact field set:
// §5.16 fixes that firing never extends or preserves authority, so the event
// carries identity and expectations only. Adding a field is a contract
// change that must update this list — and must never be a credential, token,
// or grant.
func TestScheduleEventCarriesNoAuthority(t *testing.T) {
	want := []string{
		"ScheduleID", "ProjectID", "Kind", "Generation", "Subject",
		"NominalFireAt", "FiredAt", "Gap",
	}
	typ := reflect.TypeOf(ScheduleEvent{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ScheduleEvent fields = %v, want %v", got, want)
	}
}
