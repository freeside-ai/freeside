package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func adjudicationDigest(component string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(component, 64/len(component)))
}

// adjSpecDigest, adjPolicyDigest, and adjInstructionDigest are the authoritative
// binding values for an adjudication artifact: the run's spec and policy digests
// and the review round's instruction binding. The artifact must carry the same
// values or the store's binding rejects it, so the seed run, the seed review
// record, and the fixtures share them.
var (
	adjSpecDigest        = adjudicationDigest("a")
	adjPolicyDigest      = adjudicationDigest("c")
	adjInstructionDigest = adjudicationDigest("d")
)

func adjudicationEngineEntry(t *testing.T, id domain.FindingID) domain.FindingAdjudicationEntry {
	t.Helper()
	// A deterministic fast-path fact: required goal, presumptive-allowed
	// compatibility, remediation route — the single engine-producible row (the
	// no-model fast path is one-directional toward remediation; see the domain
	// contract's engine-entry restriction).
	entry, err := domain.NewEngineAdjudicationEntry(
		id, domain.GoalRequired, ptrCompat(domain.CompatibilityAllowed), domain.RouteRemediate,
		"in declared scope", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine entry %q: %v", id, err)
	}
	return entry
}

func ptrCompat(c domain.WorkUnitCompatibility) *domain.WorkUnitCompatibility { return &c }

func newAdjudication(
	t *testing.T, runID domain.RunID, round int, findingIDs []domain.FindingID, createdAt time.Time,
) domain.FindingAdjudication {
	t.Helper()
	entries := make([]domain.FindingAdjudicationEntry, 0, len(findingIDs))
	for _, id := range findingIDs {
		entries = append(entries, adjudicationEngineEntry(t, id))
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, round,
		adjSpecDigest, adjInstructionDigest, adjPolicyDigest,
		entries, createdAt)
	if err != nil {
		t.Fatalf("new adjudication: %v", err)
	}
	return artifact
}

func adjudicationReviewRecord(
	t *testing.T, runID domain.RunID, round int, findingIDs []domain.FindingID, completedAt time.Time,
) domain.ReviewRecord {
	t.Helper()
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + string(runID) + "-" + strconv.Itoa(round)),
		RunID:        runID, Round: round, Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   adjInstructionDigest,
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head-" + strconv.Itoa(round), CompletedAt: completedAt,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatalf("review record round %d: %v", round, err)
	}
	return record
}

func adjudicationFinding(id domain.FindingID, runID domain.RunID, path string, at time.Time) domain.Finding {
	return domain.Finding{
		ID: id, RunID: runID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: path, StartLine: 1, EndLine: 1},
		Message:  "finding " + string(id), RawText: "finding " + string(id), CreatedAt: at,
	}
}

// seedReviewRound opens a store, seeds a run and one review round with the given
// findings, and returns the store for adjudication writes.
func seedReviewRound(
	t *testing.T, runID domain.RunID, round int, findings []domain.Finding, at time.Time,
) *store.Store {
	t.Helper()
	return seedReviewRoundAt(t, filepath.Join(t.TempDir(), "store.db"), runID, round, findings, at)
}

func seedReviewRoundAt(
	t *testing.T, path string, runID domain.RunID, round int, findings []domain.Finding, at time.Time,
) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ids := make([]domain.FindingID, 0, len(findings))
	for _, f := range findings {
		ids = append(ids, f.ID)
	}
	record := adjudicationReviewRecord(t, runID, round, ids, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, findings)
	}); err != nil {
		t.Fatalf("seed review round: %v", err)
	}
	return st
}

func TestFindingAdjudicationRoundTripAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-adj")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{
		adjudicationFinding("finding-a", runID, "daemon/a.go", at),
		adjudicationFinding("finding-b", runID, "daemon/b.go", at),
	}
	st := seedReviewRound(t, runID, 1, findings, at)

	artifact := newAdjudication(t, runID, 1, []domain.FindingID{"finding-b", "finding-a"}, at)

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A byte-identical replay converges.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		byDigest, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		if byDigest.Digest != artifact.Digest {
			t.Fatalf("get by digest returned %q", byDigest.Digest)
		}
		byRound, err := tx.GetFindingAdjudicationForRound(ctx, runID, 1)
		if err != nil {
			return err
		}
		if byRound.Digest != artifact.Digest {
			t.Fatalf("get by round returned %q", byRound.Digest)
		}
		list, err := tx.ListFindingAdjudications(ctx, runID)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].Digest != artifact.Digest {
			t.Fatalf("list returned %d artifacts", len(list))
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestFindingAdjudicationImmutableConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-conflict")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}
	st := seedReviewRound(t, runID, 1, findings, at)

	first := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)
	// Same round, same finding set, different content (later timestamp).
	second := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at.Add(time.Minute))
	if first.Digest == second.Digest {
		t.Fatal("fixtures share a digest; conflict test is vacuous")
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, first)
	}); err != nil {
		t.Fatalf("put first: %v", err)
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, second)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("conflicting put = %v, want ErrImmutableConflict", err)
	}
}

func TestFindingAdjudicationBindingFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-binding")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{
		adjudicationFinding("finding-a", runID, "daemon/a.go", at),
		adjudicationFinding("finding-b", runID, "daemon/b.go", at),
	}
	st := seedReviewRound(t, runID, 1, findings, at)

	cases := []struct {
		name     string
		artifact domain.FindingAdjudication
	}{
		{"missing review record", newAdjudication(t, runID, 9, []domain.FindingID{"finding-a", "finding-b"}, at)},
		{"foreign finding", newAdjudication(t, runID, 1, []domain.FindingID{"finding-a", "finding-c"}, at)},
		{"missing finding", newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, tc.artifact)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("%s put = %v, want ErrParentKeyMismatch", tc.name, err)
			}
		})
	}
}

// TestFindingAdjudicationDigestBinding proves the store re-gates every
// caller-supplied binding digest against its authority: the approved-spec and
// resolved-policy digests against the run, and the instruction snapshot against
// the review round. A syntactically valid but disagreeing digest cannot persist
// an adjudication bound to a spec, policy, or instruction snapshot the round is
// not.
func TestFindingAdjudicationDigestBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-digest-binding")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}
	st := seedReviewRound(t, runID, 1, findings, at)

	// A run-round-and-finding-set-correct artifact whose spec, policy, or
	// instruction digest disagrees with its authority must be rejected, isolating
	// each binding: every parameter but the varied one carries its correct value.
	newBound := func(t *testing.T, spec, instruction, policy domain.Digest) domain.FindingAdjudication {
		t.Helper()
		entry := adjudicationEngineEntry(t, "finding-a")
		artifact, err := domain.NewFindingAdjudication(
			runID, 1, spec, instruction, policy,
			[]domain.FindingAdjudicationEntry{entry}, at)
		if err != nil {
			t.Fatalf("new adjudication: %v", err)
		}
		return artifact
	}

	cases := []struct {
		name     string
		artifact domain.FindingAdjudication
	}{
		{"spec digest mismatch", newBound(t, adjudicationDigest("f"), adjInstructionDigest, adjPolicyDigest)},
		{"policy digest mismatch", newBound(t, adjSpecDigest, adjInstructionDigest, adjudicationDigest("f"))},
		{"instruction digest mismatch", newBound(t, adjSpecDigest, adjudicationDigest("f"), adjPolicyDigest)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, tc.artifact)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("%s put = %v, want ErrParentKeyMismatch", tc.name, err)
			}
		})
	}
}

func TestFindingAdjudicationStoredBodyGolden(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-golden")
	at := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	artifact := newAdjudication(t, runID, 2, []domain.FindingID{"finding-0001", "finding-0002"}, at)
	body, err := artifact.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Re-indent the persisted compact body for a readable, stable golden.
	var pretty json.RawMessage = body
	indented, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		t.Fatalf("indent: %v", err)
	}
	golden.Assert(t, "finding_adjudication", append(indented, '\n'))
}
