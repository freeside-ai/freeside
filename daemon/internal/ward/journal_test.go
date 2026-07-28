package ward

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// fakeJournal is an in-memory HandoffJournal holding the interface's own
// contract: one record per run ever, durable-before-return is trivially true
// in memory, amendments require an open record, and Close is terminal.
// Per-method error injection scripts journal failures; the call log records
// write ordering for intent-before-create assertions.
type fakeJournal struct {
	mu      sync.Mutex
	records map[string]*HandoffJournalRecord
	calls   []string
	// rt, when set, mirrors journal calls into the fake runtime's shared
	// call log so tests can assert write ordering against runtime
	// operations (intent-before-create).
	rt *fakeRuntime
	// onCall, when set, observes each journal call before the method
	// mutates state; the kill-boundary harness snapshots there, modeling a
	// daemon that died with the write not yet durable. It runs on the
	// handoff goroutine, so unlocked reads of test state are safe.
	onCall func(call string)

	failBegin, failMark, failClose error
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{records: map[string]*HandoffJournalRecord{}}
}

func (j *fakeJournal) recordCall(s string) {
	j.calls = append(j.calls, s)
	if j.rt != nil {
		j.rt.mu.Lock()
		j.rt.calls = append(j.rt.calls, s)
		j.rt.mu.Unlock()
	}
	if j.onCall != nil {
		j.onCall(s)
	}
}

// snapshot returns a deep copy of the run's record, or nil.
func (j *fakeJournal) snapshot(runID string) *HandoffJournalRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec, ok := j.records[runID]
	if !ok {
		return nil
	}
	cp := *rec
	if rec.Lease != nil {
		lease := *rec.Lease
		cp.Lease = &lease
	}
	if rec.Outcome != nil {
		outcome := *rec.Outcome
		cp.Outcome = &outcome
	}
	return &cp
}

func (j *fakeJournal) Get(_ context.Context, runID string) (HandoffJournalRecord, error) {
	j.recordCall("journal-get " + runID)
	rec := j.snapshot(runID)
	if rec == nil {
		return HandoffJournalRecord{}, errors.New("fake journal: no record for run")
	}
	return *rec, nil
}

// put overwrites the run's stored record directly; tests use it to model a
// tampered or diverged durable row.
func (j *fakeJournal) put(rec HandoffJournalRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := rec
	j.records[rec.RunID] = &cp
}

func (j *fakeJournal) Begin(_ context.Context, rec HandoffJournalRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordCall("journal-begin " + rec.RunID)
	if j.failBegin != nil {
		return j.failBegin
	}
	if _, exists := j.records[rec.RunID]; exists {
		return errors.New("fake journal: record already exists for run")
	}
	cp := rec
	j.records[rec.RunID] = &cp
	return nil
}

func (j *fakeJournal) open(runID string) (*HandoffJournalRecord, error) {
	rec, ok := j.records[runID]
	if !ok {
		return nil, errors.New("fake journal: no record for run")
	}
	if rec.Outcome != nil {
		return nil, errors.New("fake journal: record is closed")
	}
	return rec, nil
}

func (j *fakeJournal) MarkSeedObserved(_ context.Context, runID, observedBaseSHA string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordCall("journal-seed-observed " + runID)
	if j.failMark != nil {
		return j.failMark
	}
	rec, err := j.open(runID)
	if err != nil {
		return err
	}
	rec.ObservedBaseSHA = observedBaseSHA
	return nil
}

func (j *fakeJournal) MarkCredentialObserved(_ context.Context, runID, preDigest string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordCall("journal-cred-observed " + runID)
	if j.failMark != nil {
		return j.failMark
	}
	rec, err := j.open(runID)
	if err != nil {
		return err
	}
	rec.CredentialPreDigest = preDigest
	return nil
}

func (j *fakeJournal) MarkWriterComplete(_ context.Context, runID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordCall("journal-writer-complete " + runID)
	if j.failMark != nil {
		return j.failMark
	}
	rec, err := j.open(runID)
	if err != nil {
		return err
	}
	rec.WriterComplete = true
	return nil
}

func (j *fakeJournal) MarkExportMaterialized(_ context.Context, runID, exportDir string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordCall("journal-export-materialized " + runID)
	if j.failMark != nil {
		return j.failMark
	}
	rec, err := j.open(runID)
	if err != nil {
		return err
	}
	rec.ExportDir = exportDir
	return nil
}

func (j *fakeJournal) Close(_ context.Context, runID string, outcome HandoffJournalOutcome) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recordCall("journal-close " + runID + " " + string(outcome))
	if j.failClose != nil {
		return j.failClose
	}
	rec, err := j.open(runID)
	if err != nil {
		return err
	}
	o := outcome
	rec.Outcome = &o
	return nil
}

// TestFakeJournalContract pins the fake to the HandoffJournal interface's
// own rules, so the handoff and recovery tests that run over it exercise the
// contract a real store adapter must hold: one record per run ever,
// amendments only on an open record, Close terminal, and injected failures
// surfaced verbatim.
func TestFakeJournalContract(t *testing.T) {
	ctx := context.Background()
	j := newFakeJournal()
	rec := testJournalRecord()
	rec.Outcome = nil
	rec.WriterComplete = false

	if err := j.Begin(ctx, rec); err != nil {
		t.Fatalf("Begin = %v, want nil", err)
	}
	if err := j.Begin(ctx, rec); err == nil {
		t.Fatal("second Begin for the same run succeeded; one record per run, ever")
	}
	if err := j.MarkWriterComplete(ctx, rec.RunID); err != nil {
		t.Fatalf("MarkWriterComplete = %v, want nil", err)
	}
	if err := j.MarkSeedObserved(ctx, "other-run", strings.Repeat("12", 20)); err == nil {
		t.Fatal("amendment for an unopened run succeeded")
	}
	if err := j.Close(ctx, rec.RunID, HandoffLoss); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if err := j.Close(ctx, rec.RunID, HandoffLoss); err == nil {
		t.Fatal("second Close succeeded; Close is terminal")
	}
	if err := j.MarkCredentialObserved(ctx, rec.RunID, strings.Repeat("cd", 32)); err == nil {
		t.Fatal("amendment after Close succeeded")
	}
	snap := j.snapshot(rec.RunID)
	if snap == nil || !snap.WriterComplete || snap.Outcome == nil || *snap.Outcome != HandoffLoss {
		t.Errorf("snapshot = %+v, want writer-complete, closed as loss", snap)
	}
	if j.snapshot("other-run") != nil {
		t.Error("snapshot of an unopened run is non-nil")
	}

	inject := errors.New("fixture: journal write failed")
	j2 := newFakeJournal()
	j2.failBegin = inject
	if err := j2.Begin(ctx, rec); !errors.Is(err, inject) {
		t.Errorf("injected Begin failure = %v, want the injected error", err)
	}
	j3 := newFakeJournal()
	if err := j3.Begin(ctx, rec); err != nil {
		t.Fatalf("Begin = %v, want nil", err)
	}
	j3.failMark = inject
	j3.failClose = inject
	if err := j3.MarkWriterComplete(ctx, rec.RunID); !errors.Is(err, inject) {
		t.Errorf("injected mark failure = %v, want the injected error", err)
	}
	if err := j3.Close(ctx, rec.RunID, HandoffCompleted); !errors.Is(err, inject) {
		t.Errorf("injected close failure = %v, want the injected error", err)
	}
	if got := len(j.calls); got == 0 {
		t.Error("call log recorded nothing")
	}
}

// testJournalRecord is the fixed valid record fixture; tests copy and mutate.
func testJournalRecord() HandoffJournalRecord {
	completed := HandoffCompleted
	return HandoffJournalRecord{
		RunID:               "golden-run",
		OwnershipToken:      "00112233445566778899aabbccddeeff",
		SpecDigest:          strings.Repeat("ab", 32),
		ObservedBaseSHA:     strings.Repeat("12", 20),
		CredentialPreDigest: strings.Repeat("cd", 32),
		WriterComplete:      true,
		Lease: &HandoffJournalLease{
			AuthIdentityID: "identity-fixture",
			Holder:         "holder-fixture",
			Fence:          1,
			AcquiredAt:     time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
			ExpiresAt:      time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
		},
		ExportDir: "/tmp/freeside-handoff-golden-run-out-fixture",
		Outcome:   &completed,
		OpenedAt:  time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
}

// TestHandoffJournalRecordGolden pins the record's contract shape: it is the
// vocabulary the #237 store adapter persists, so a drift must be a reviewed
// diff.
func TestHandoffJournalRecordGolden(t *testing.T) {
	got, err := json.MarshalIndent(testJournalRecord(), "", "  ")
	if err != nil {
		t.Fatalf("marshal journal record: %v", err)
	}
	golden.Assert(t, "journal-record", append(got, '\n'))
}

// TestHandoffJournalRecordValidate enumerates the reconstruction re-gate: a
// record read back after a restart is untrusted input, and every malformed
// shape is refused before it can steer recovery.
func TestHandoffJournalRecordValidate(t *testing.T) {
	if err := testJournalRecord().validate(); err != nil {
		t.Fatalf("valid fixture: validate() = %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*HandoffJournalRecord)
	}{
		{"empty run id", func(r *HandoffJournalRecord) { r.RunID = "" }},
		{"uppercase run id", func(r *HandoffJournalRecord) { r.RunID = "Golden-Run" }},
		{"empty ownership token", func(r *HandoffJournalRecord) { r.OwnershipToken = "" }},
		{"short ownership token", func(r *HandoffJournalRecord) { r.OwnershipToken = "abc123" }},
		{"uppercase ownership token", func(r *HandoffJournalRecord) {
			r.OwnershipToken = strings.ToUpper(r.OwnershipToken)
		}},
		{"empty spec digest", func(r *HandoffJournalRecord) { r.SpecDigest = "" }},
		{"short spec digest", func(r *HandoffJournalRecord) { r.SpecDigest = strings.Repeat("ab", 20) }},
		{"malformed observed base", func(r *HandoffJournalRecord) { r.ObservedBaseSHA = "HEAD" }},
		{"malformed credential pre-digest", func(r *HandoffJournalRecord) { r.CredentialPreDigest = "xyz" }},
		{"lease without identity", func(r *HandoffJournalRecord) { r.Lease.AuthIdentityID = "" }},
		{"lease without holder", func(r *HandoffJournalRecord) { r.Lease.Holder = "" }},
		{"lease with zero fence", func(r *HandoffJournalRecord) { r.Lease.Fence = 0 }},
		{"lease without a window", func(r *HandoffJournalRecord) { r.Lease.AcquiredAt = time.Time{} }},
		{"lease window without expiry", func(r *HandoffJournalRecord) { r.Lease.ExpiresAt = time.Time{} }},
		{"lease window expiring at acquisition", func(r *HandoffJournalRecord) { r.Lease.ExpiresAt = r.Lease.AcquiredAt }},
		{"relative export dir", func(r *HandoffJournalRecord) { r.ExportDir = "exports/run" }},
		{"unknown outcome", func(r *HandoffJournalRecord) {
			bad := HandoffJournalOutcome("abandoned")
			r.Outcome = &bad
		}},
		{"empty outcome pointer", func(r *HandoffJournalRecord) {
			empty := HandoffJournalOutcome("")
			r.Outcome = &empty
		}},
		{"zero opened_at", func(r *HandoffJournalRecord) { r.OpenedAt = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testJournalRecord()
			// The fixture shares pointers; re-point them before mutation.
			lease := *r.Lease
			r.Lease = &lease
			tc.mutate(&r)
			if err := r.validate(); !errors.Is(err, ErrInvalidJournalRecord) {
				t.Errorf("validate() = %v, want ErrInvalidJournalRecord", err)
			}
		})
	}

	// An open record (no outcome, no amendments yet) is also valid: empty
	// observed base, pre-digest, and lease are legitimate pre-amendment
	// states, not violations.
	open := testJournalRecord()
	open.ObservedBaseSHA = ""
	open.CredentialPreDigest = ""
	open.WriterComplete = false
	open.Lease = nil
	open.ExportDir = ""
	open.Outcome = nil
	if err := open.validate(); err != nil {
		t.Errorf("open record: validate() = %v, want nil", err)
	}
}

// TestSpecDigest pins the digest's two properties: deterministic for equal
// specs, distinct for any spec difference recovery must not conflate.
func TestSpecDigest(t *testing.T) {
	a, err := specDigest(testHandoffSpec())
	if err != nil {
		t.Fatalf("specDigest: %v", err)
	}
	b, err := specDigest(testHandoffSpec())
	if err != nil {
		t.Fatalf("specDigest: %v", err)
	}
	if a != b {
		t.Errorf("equal specs digest differently: %q vs %q", a, b)
	}
	if !sha256HexPattern.MatchString(a) {
		t.Errorf("digest %q is not sha256 hex", a)
	}

	mutations := map[string]func(*HandoffSpec){
		"run id":         func(s *HandoffSpec) { s.RunID = "other-run" },
		"agent command":  func(s *HandoffSpec) { s.Agent.Command = []string{"sh", "-c", "false"} },
		"mount writable": func(s *HandoffSpec) { s.Agent.CredentialMounts[0].Writable = true },
		"lease claim": func(s *HandoffSpec) {
			s.AuthStoreLease = &AuthStoreLeaseClaim{AuthIdentityID: "id", Holder: "h"}
		},
	}
	for name, mutate := range mutations {
		s := testHandoffSpec()
		mutate(&s)
		got, err := specDigest(s)
		if err != nil {
			t.Fatalf("specDigest(%s): %v", name, err)
		}
		if got == a {
			t.Errorf("spec differing in %s digests identically", name)
		}
	}
}
