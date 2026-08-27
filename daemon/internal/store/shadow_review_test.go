package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func shadowRecord(
	t *testing.T, runID domain.RunID, invocationID domain.InvocationID,
	round int, findings []domain.FindingID, at time.Time,
) domain.ShadowReviewRecord {
	t.Helper()
	outcome := domain.ReviewClean
	if len(findings) > 0 {
		outcome = domain.ReviewFindings
	}
	record, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: invocationID, RunID: runID, ShadowedRound: round,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration:  "claude-opus/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head",
		CompletedAt: at, CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome: outcome, FindingIDs: findings,
	})
	if err != nil {
		t.Fatalf("new shadow record: %v", err)
	}
	return record
}

func routedCandidate(
	t *testing.T, runID domain.RunID, invocationID domain.InvocationID,
	round int, findings []domain.FindingID, at time.Time,
) domain.ReviewRecord {
	t.Helper()
	outcome := domain.ReviewClean
	if len(findings) > 0 {
		outcome = domain.ReviewFindings
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: invocationID, RunID: runID, Round: round,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            outcome, FindingIDs: findings,
	})
	if err != nil {
		t.Fatalf("new routed candidate: %v", err)
	}
	return record
}

func TestCleanShadowReviewCannotSatisfyRoutedReviewEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{
		ID: "run-shadow-clean", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	at := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	record := shadowRecord(t, run.ID, "shadow-clean-1", 1, nil, at)
	routedFinding := domain.Finding{
		ID: "routed-finding-1", RunID: run.ID, Source: "codex_local", Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
		Message:  "routed finding", RawText: "routed finding", CreatedAt: at,
	}
	routed := routedCandidate(t, run.ID, "routed-findings-1", 1,
		[]domain.FindingID{routedFinding.ID}, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, []domain.Finding{routedFinding}); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetShadowReviewRecord(ctx, record.InvocationID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, record) {
			t.Fatalf("shadow record = %#v, want %#v", got, record)
		}
		shadowHistory, err := tx.ListShadowReviewRecords(ctx, run.ID)
		if err != nil {
			return err
		}
		if len(shadowHistory) != 1 || shadowHistory[0].InvocationID != record.InvocationID {
			t.Fatalf("shadow history = %#v", shadowHistory)
		}
		// assertReviewedCandidate and latestReviewState consume only these two
		// routed-review accessors. A clean shadow pass is absent from both, so it
		// cannot satisfy publication readiness or advance the routed round.
		routedHistory, err := tx.ListReviewRecords(ctx, run.ID)
		if err != nil {
			return err
		}
		if len(routedHistory) != 1 || !reflect.DeepEqual(routedHistory[0], routed) {
			t.Fatalf("routed review history = %#v, want only %#v", routedHistory, routed)
		}
		latest, err := tx.LatestReviewRecord(ctx, run.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(latest, routed) || latest.Outcome != domain.ReviewFindings {
			t.Fatalf("latest routed review = %#v, want findings record %#v", latest, routed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestShadowReviewRejectsFindingOutsideSourceSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.Finding)
		want   error
	}{
		{name: "empty severity", mutate: func(f *domain.Finding) { f.Severity = "" }, want: domain.ErrInvalidFindingSeverity},
		{name: "missing location", mutate: func(f *domain.Finding) { f.Location = nil }, want: domain.ErrEmptyField},
		{name: "partial range", mutate: func(f *domain.Finding) {
			f.Location = &domain.FindingLocation{Path: "daemon/main.go", EndLine: 4}
		}, want: domain.ErrNonPositive},
		{name: "empty explanation", mutate: func(f *domain.Finding) { f.Message = "" }, want: domain.ErrEmptyField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := openStore(t, store.Options{})
			suffix := strings.ReplaceAll(tc.name, " ", "-")
			run := domain.Run{
				ID: domain.RunID("run-shadow-schema-" + suffix), ProjectID: "project-1",
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}
			finding := domain.Finding{
				ID: domain.FindingID("finding-shadow-schema-" + suffix), RunID: run.ID,
				Source: string(domain.ShadowReviewClaudeLocal), Severity: domain.FindingSeverityP2,
				Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 4, EndLine: 4},
				Message:  "unchecked error", RawText: "unchecked error", CreatedAt: at,
			}
			tc.mutate(&finding)
			routed := routedCandidate(t, run.ID, "routed-schema-1", 1, nil, at)
			shadow := shadowRecord(t, run.ID, "shadow-schema-1", 1,
				[]domain.FindingID{finding.ID}, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				return tx.PutReviewRecord(ctx, routed, nil)
			}); err != nil {
				t.Fatal(err)
			}
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("invalid shadow finding write = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestShadowReviewPersistsWholeFileFinding is the #855 regression guard at the
// store trust boundary: a candidate-deleted-file shadow finding carries the
// whole-file location (0,0), and both persistence boundaries — the
// PutShadowReviewRecord write and GetShadowReviewRecord's re-validation on read
// — must admit it in lockstep with the shared ward normalization, not discard
// the pass as a non-positive range.
func TestShadowReviewPersistsWholeFileFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)
	st := openStore(t, store.Options{})
	run := domain.Run{
		ID: "run-shadow-wholefile", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	finding := domain.Finding{
		ID: "finding-shadow-wholefile", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "daemon/gone.go"},
		Message:  "deletes the only caller", RawText: "deletes the only caller", CreatedAt: at,
	}
	routed := routedCandidate(t, run.ID, "routed-wholefile-1", 1, nil, at)
	shadow := shadowRecord(t, run.ID, "shadow-wholefile-1", 1,
		[]domain.FindingID{finding.ID}, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
	}); err != nil {
		t.Fatalf("whole-file shadow finding rejected at persistence: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetShadowReviewRecord(ctx, shadow.InvocationID); err != nil {
			return err
		}
		got, err := tx.GetFinding(ctx, finding.ID)
		if err != nil {
			return err
		}
		want := domain.FindingLocation{Path: "daemon/gone.go"}
		if got.Location == nil || *got.Location != want {
			t.Fatalf("reconstructed whole-file location = %#v, want %#v", got.Location, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifierAccuracySampleRoundTripAndStableJoins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{
		ID: "run-shadow-sample", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	at := time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)
	finding := domain.Finding{
		ID: "shadow-finding-1", RunID: run.ID, Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 12, EndLine: 12},
		Message:  "unchecked error", RawText: "unchecked error", CreatedAt: at,
	}
	record := shadowRecord(t, run.ID, "shadow-findings-1", 1, []domain.FindingID{finding.ID}, at)
	routed := routedCandidate(t, run.ID, "routed-sample-1", 1, nil, at)
	classification := domain.Classification{
		FindingID: finding.ID, Version: 1, Materiality: "medium", Confidence: "high", Note: "sampled",
	}
	sample := domain.ClassifierAccuracySample{
		RunID: run.ID, FindingID: finding.ID, ClassificationVersion: classification.Version,
		ShadowInvocationID: record.InvocationID,
		Assessment:         domain.ClassifierAssessmentAccurate,
		RecordedAt:         at.Add(time.Minute),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		if err := tx.PutShadowReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutClassification(ctx, classification); err != nil {
			return err
		}
		return tx.PutClassifierAccuracySample(ctx, sample)
	}); err != nil {
		t.Fatal(err)
	}
	// A byte-identical replay converges.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutClassifierAccuracySample(ctx, sample)
	}); err != nil {
		t.Fatalf("sample replay: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		samples, err := tx.ListClassifierAccuracySamples(ctx, run.ID)
		if err != nil {
			return err
		}
		if len(samples) != 1 || !reflect.DeepEqual(samples[0], sample) {
			t.Fatalf("samples = %#v, want %#v", samples, sample)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed := sample
	changed.Assessment = domain.ClassifierAssessmentInaccurate
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutClassifierAccuracySample(ctx, changed)
	}); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("conflicting sample = %v, want ErrImmutableConflict", err)
	}
}

func TestClassifierAccuracySampleRejectsDanglingShadowJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	sample := domain.ClassifierAccuracySample{
		RunID: "run-missing-shadow", FindingID: "finding-missing", ClassificationVersion: 1,
		ShadowInvocationID: "shadow-missing",
		Assessment:         domain.ClassifierAssessmentIndeterminate,
		RecordedAt:         time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC),
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutClassifierAccuracySample(ctx, sample)
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dangling shadow sample = %v, want ErrNotFound", err)
	}
}

func TestShadowReviewRejectsMismatchedRoutedCandidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ShadowReviewRecord)
	}{
		{name: "missing routed round", mutate: func(record *domain.ShadowReviewRecord) {
			record.ShadowedRound++
		}},
		{name: "different base", mutate: func(record *domain.ShadowReviewRecord) {
			record.BaseSHA = "other-base"
		}},
		{name: "different head", mutate: func(record *domain.ShadowReviewRecord) {
			record.HeadSHA = "other-head"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := openStore(t, store.Options{})
			run := domain.Run{
				ID:        domain.RunID("run-shadow-candidate-" + strings.ReplaceAll(tc.name, " ", "-")),
				ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}
			routed := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
			shadow := shadowRecord(t, run.ID, "shadow-candidate-1", 1, nil, at)
			tc.mutate(&shadow)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutRun(ctx, run); err != nil {
					return err
				}
				return tx.PutReviewRecord(ctx, routed, nil)
			}); err != nil {
				t.Fatal(err)
			}
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("mismatched routed candidate = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestShadowAndRoutedReviewRejectDuplicateInvocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 5, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *store.Store, domain.Run, time.Time) error
	}{
		{name: "routed then shadow", run: func(t *testing.T, st *store.Store, run domain.Run, at time.Time) error {
			routed := routedCandidate(t, run.ID, "shared-invocation", 1, nil, at)
			shadow := shadowRecord(t, run.ID, "shared-invocation", 1, nil, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewRecord(ctx, routed, nil)
			}); err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			})
		}},
		{name: "failure then shadow", run: func(t *testing.T, st *store.Store, run domain.Run, at time.Time) error {
			routed := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
			failure := domain.ReviewFailure{
				InvocationID: "shared-invocation", RunID: run.ID, Round: 2,
				BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureTransient,
				Reason: "retry exhausted", ObservedAt: at,
			}
			shadow := shadowRecord(t, run.ID, "shared-invocation", 1, nil, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
					return err
				}
				return tx.PutReviewFailure(ctx, failure)
			}); err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			})
		}},
		{name: "retry then shadow", run: func(t *testing.T, st *store.Store, run domain.Run, at time.Time) error {
			candidate := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
			retry := domain.ReviewRetry{
				RunID: run.ID, InvocationID: "shared-invocation", Round: 2,
				BaseSHA: "base", HeadSHA: "head", ObservedAt: at, Reason: "transient poll failure",
			}
			shadow := shadowRecord(t, run.ID, "shared-invocation", 1, nil, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
					return err
				}
				return tx.PutReviewRetry(ctx, retry)
			}); err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			})
		}},
		{name: "shadow then routed", run: func(t *testing.T, st *store.Store, run domain.Run, at time.Time) error {
			candidate := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
			shadow := shadowRecord(t, run.ID, "shared-invocation", 1, nil, at)
			routed := routedCandidate(t, run.ID, "shared-invocation", 2, nil, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
					return err
				}
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			}); err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewRecord(ctx, routed, nil)
			})
		}},
		{name: "shadow then failure", run: func(t *testing.T, st *store.Store, run domain.Run, at time.Time) error {
			candidate := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
			shadow := shadowRecord(t, run.ID, "shared-invocation", 1, nil, at)
			failure := domain.ReviewFailure{
				InvocationID: "shared-invocation", RunID: run.ID, Round: 2,
				BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureTransient,
				Reason: "retry exhausted", ObservedAt: at,
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
					return err
				}
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			}); err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewFailure(ctx, failure)
			})
		}},
		{name: "shadow then retry", run: func(t *testing.T, st *store.Store, run domain.Run, at time.Time) error {
			candidate := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
			shadow := shadowRecord(t, run.ID, "shared-invocation", 1, nil, at)
			retry := domain.ReviewRetry{
				RunID: run.ID, InvocationID: "shared-invocation", Round: 2,
				BaseSHA: "base", HeadSHA: "head", ObservedAt: at, Reason: "transient poll failure",
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
					return err
				}
				return tx.PutShadowReviewRecord(ctx, shadow, nil)
			}); err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewRetry(ctx, retry)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := openStore(t, store.Options{})
			run := domain.Run{
				ID:        domain.RunID("run-duplicate-invocation-" + strings.ReplaceAll(tc.name, " ", "-")),
				ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
				t.Fatal(err)
			}
			if err := tc.run(t, st, run, at); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("duplicate invocation write = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestShadowReviewRejectsFindingFromAnotherShadowPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	at := time.Date(2026, 8, 24, 5, 45, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-shadow-parent", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	finding := domain.Finding{
		ID: "finding-shadow-parent", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: "P2",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
		Message:  "finding", RawText: "finding", CreatedAt: at,
	}
	firstRouted := routedCandidate(t, run.ID, "routed-parent-1", 1, nil, at)
	secondRouted := routedCandidate(t, run.ID, "routed-parent-2", 2, nil, at)
	first := shadowRecord(t, run.ID, "shadow-parent-1", 1, []domain.FindingID{finding.ID}, at)
	second := shadowRecord(t, run.ID, "shadow-parent-2", 2, []domain.FindingID{finding.ID}, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, firstRouted, nil); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, secondRouted, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, first, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutShadowReviewRecord(ctx, second, []domain.Finding{finding})
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("second shadow parent = %v, want ErrParentKeyMismatch", err)
	}
}

func TestShadowAndRoutedReviewRejectDualLinkedFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 24, 6, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		shadowFirst bool
	}{
		{name: "shadow then routed", shadowFirst: true},
		{name: "routed then shadow", shadowFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := openStore(t, store.Options{})
			run := domain.Run{
				ID: domain.RunID("run-dual-" + strings.ReplaceAll(tc.name, " ", "-")), ProjectID: "project-1",
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}
			finding := domain.Finding{
				ID:    domain.FindingID("finding-dual-" + strings.ReplaceAll(tc.name, " ", "-")),
				RunID: run.ID, Source: string(domain.ShadowReviewClaudeLocal), Severity: "P1",
				Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
				Message:  "dual-linked finding", RawText: "dual-linked finding", CreatedAt: at,
			}
			shadow := shadowRecord(t, run.ID, "shadow-dual-1", 1, []domain.FindingID{finding.ID}, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
				t.Fatal(err)
			}
			if tc.shadowFirst {
				candidate := routedCandidate(t, run.ID, "routed-candidate-1", 1, nil, at)
				if err := st.Write(ctx, func(tx *store.WriteTx) error {
					if err := tx.PutReviewRecord(ctx, candidate, nil); err != nil {
						return err
					}
					return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
				}); err != nil {
					t.Fatalf("shadow lane write: %v", err)
				}
				routed := routedCandidate(t, run.ID, "routed-dual-2", 2,
					[]domain.FindingID{finding.ID}, at)
				err := st.Write(ctx, func(tx *store.WriteTx) error {
					return tx.PutReviewRecord(ctx, routed, []domain.Finding{finding})
				})
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("dual-link write = %v, want ErrParentKeyMismatch", err)
				}
				return
			}
			routed := routedCandidate(t, run.ID, "routed-dual-1", 1,
				[]domain.FindingID{finding.ID}, at)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewRecord(ctx, routed, []domain.Finding{finding})
			}); err != nil {
				t.Fatalf("routed lane write: %v", err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
			}); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("dual-link write = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}
