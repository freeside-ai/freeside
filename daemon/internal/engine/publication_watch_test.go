package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func watchTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func watchTestItem(t *testing.T, st *store.Store, status domain.ItemStatus) domain.AttentionItem {
	t.Helper()
	runID := domain.RunID("run-1")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: "sha256:watch-test-policy",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-1", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: status,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *store.WriteTx) error {
		ctx := context.Background()
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: item.ProjectID,
			SpecDigest: "sha256:watch-test-spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// TestArmPublicationWatches: a live ready item arms exactly the three
// §5.16 watch kinds bound to the item and its version, the ensure is
// convergent across reconcile passes (a later pass with a fresh clock must
// not re-derive an armed deadline), and a concluded item arms nothing.
func TestArmPublicationWatches(t *testing.T) {
	ctx := context.Background()
	st := watchTestStore(t)
	item := watchTestItem(t, st, domain.StatusOpen)
	now := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)

	if err := armPublicationWatches(ctx, st, item, "owner/repo", "main", "cafebabe", now); err != nil {
		t.Fatal(err)
	}
	var schedules []store.Snapshotted[domain.Schedule]
	readAll := func() {
		t.Helper()
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			schedules, err = tx.ListSchedules(ctx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	readAll()
	if len(schedules) != 3 {
		t.Fatalf("schedules = %d", len(schedules))
	}
	byKind := map[domain.ScheduleKind]domain.Schedule{}
	for _, s := range schedules {
		byKind[s.Value.Kind] = s.Value
		if *s.Value.Subject.ItemID != item.ID || *s.Value.Subject.ItemVersion != item.ItemVersion {
			t.Fatalf("subject binding = %+v", s.Value.Subject)
		}
	}
	deadline := byKind[domain.SchedulePRChecksDeadline]
	if !deadline.FireAt.Equal(now.Add(DefaultPRChecksDeadline)) {
		t.Fatalf("checks deadline = %s", deadline.FireAt)
	}
	review := byKind[domain.ScheduleReviewWaitThreshold]
	if !review.FireAt.Equal(now.Add(DefaultReviewWaitThreshold)) {
		t.Fatalf("review threshold = %s", review.FireAt)
	}
	watch := byKind[domain.ScheduleBaseAdvanceWatch]
	if watch.BaseWatch == nil || watch.BaseWatch.AdmittedBaseSHA != "cafebabe" ||
		*watch.IntervalSeconds != int64(DefaultBaseAdvanceInterval/time.Second) {
		t.Fatalf("base watch = %+v", watch)
	}

	// A later reconcile pass re-ensures with a fresh clock: the armed
	// deadlines keep their original instants.
	if err := armPublicationWatches(ctx, st, item, "owner/repo", "main", "cafebabe", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	readAll()
	for _, s := range schedules {
		if s.Value.FireAt != nil && !s.Value.FireAt.Equal(*byKind[s.Value.Kind].FireAt) {
			t.Fatalf("re-ensure moved %s deadline to %s", s.Value.Kind, s.Value.FireAt)
		}
		if s.Snapshot.EntityVersion != 1 {
			t.Fatalf("re-ensure churned %s to entity version %d", s.Value.Kind, s.Snapshot.EntityVersion)
		}
	}
}

func TestArmPublicationWatchesSkipsConcludedItem(t *testing.T) {
	ctx := context.Background()
	st := watchTestStore(t)
	item := watchTestItem(t, st, domain.StatusResolved)
	if err := armPublicationWatches(ctx, st, item, "owner/repo", "main", "cafebabe",
		time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		schedules, err := tx.ListSchedules(ctx)
		if err != nil {
			return err
		}
		if len(schedules) != 0 {
			t.Fatalf("concluded item armed %d schedules", len(schedules))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCompatibleTerminalItemIgnoresBaseFreshness: the base-advance watch
// maintains its fact after the ready item commits, so terminal-item
// recovery must not reject an otherwise matching item that gained (or
// advanced) the fact while the daemon was down.
func TestCompatibleTerminalItemIgnoresBaseFreshness(t *testing.T) {
	st := watchTestStore(t)
	expected := watchTestItem(t, st, domain.StatusOpen)
	current := expected
	current.ItemVersion = expected.ItemVersion + 1
	current.BaseFreshness = &domain.BaseFreshness{
		BaseRef: "main", AdmittedBaseSHA: "cafebabe", ObservedBaseSHA: "deadbeef",
		Advanced: true, ObservedAt: time.Date(2026, 2, 3, 5, 0, 0, 0, time.UTC),
	}
	if !compatibleTerminalItem(expected, current) {
		t.Fatal("fact-bearing item rejected during recovery compatibility")
	}
	foreign := current
	foreign.Reason = "a different item"
	if compatibleTerminalItem(expected, foreign) {
		t.Fatal("genuinely different item accepted")
	}
}

// TestCompatibleTerminalItemIgnoresReadinessInvalidation: the invalidation
// fact is recorded in the same transition that supersedes the ready item
// (§7, issue #496), so a crash seam can leave the persisted item superseded
// with the fact set before finishTask runs. Terminal-item recovery must treat
// that item as compatible with the freshly derived ready shape (it does not
// carry the fact) instead of failing "disagrees" and stranding the task.
func TestCompatibleTerminalItemIgnoresReadinessInvalidation(t *testing.T) {
	st := watchTestStore(t)
	expected := watchTestItem(t, st, domain.StatusOpen)
	current := expected
	current.ItemVersion = expected.ItemVersion + 1
	current.Status = domain.StatusSuperseded
	current.ReadinessInvalidation = &domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged,
		Bound:  "cafebabe", Observed: "deadbeef",
		ObservedAt: time.Date(2026, 2, 3, 5, 0, 0, 0, time.UTC),
	}
	if !compatibleTerminalItem(expected, current) {
		t.Fatal("invalidation-bearing superseded item rejected during recovery compatibility")
	}
	foreign := current
	foreign.Reason = "a different item"
	if compatibleTerminalItem(expected, foreign) {
		t.Fatal("genuinely different item accepted")
	}
}

func TestCompatibleTerminalItemIgnoresSameVersionPersistenceProjections(t *testing.T) {
	st := watchTestStore(t)
	expected := watchTestItem(t, st, domain.StatusOpen)
	current := expected
	current.DecisionSurface = domain.DecisionSurfaceRef{
		Epoch: 1, Digest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
	}
	current.Recommendation = &domain.Recommendation{
		Action: domain.ActionOpenPR, Reason: "Open the verified pull request.",
		Source: domain.RecommendationDaemonPolicy,
		Provenance: domain.RecommendationProvenance{
			DaemonPolicy: &domain.DaemonPolicyRecommendationProvenance{
				RuleDigest:  domain.Digest("sha256:" + strings.Repeat("e", 64)),
				InputDigest: domain.Digest("sha256:" + strings.Repeat("f", 64)),
			},
		},
	}
	if !compatibleTerminalItem(expected, current) {
		t.Fatal("same-version persistence projections were treated as lifecycle drift")
	}
}

func TestCompatibleTerminalItemAcceptsLegacyNilReadinessOnly(t *testing.T) {
	st := watchTestStore(t)
	current := watchTestItem(t, st, domain.StatusOpen)
	expected := current
	expected.Readiness = &domain.ReadinessSummary{
		Class: domain.ReadinessReadyClean, EvaluationSetDigest: "sha256:evaluation",
	}
	if !compatibleTerminalItem(expected, current) {
		t.Fatal("legacy nil readiness rejected during recovery compatibility")
	}

	foreign := current
	foreign.Readiness = &domain.ReadinessSummary{
		Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:foreign",
	}
	if compatibleTerminalItem(expected, foreign) {
		t.Fatal("conflicting persisted readiness accepted during recovery compatibility")
	}
}
