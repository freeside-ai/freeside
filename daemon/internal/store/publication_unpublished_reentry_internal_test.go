package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// unpublishedReentryFixture is a run that was reviewed clean at reentryBase1
// and never published.
type unpublishedReentryFixture struct {
	st    *Store
	runID domain.RunID
	prior domain.ReviewRecord
}

// seedUnpublishedReentry leaves the first publication's intent slot in
// slotKind: a bare reservation, a committed intent, or absent when empty.
func seedUnpublishedReentry(t *testing.T, slotKind string) unpublishedReentryFixture {
	t.Helper()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}})
	runID := domain.RunID("run-unpublished-reentry")
	invocationID := domain.InvocationID("inv-unpublished-reentry")
	stageID := domain.StageID("stage-unpublished-reentry")
	attemptID := domain.AttemptID("attempt-unpublished-reentry")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: readyAnchorSpecDigest, PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "unpublished-reentry-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "main", BaseSHA: reentryBase1},
		Workspace: "workspace", AdmittedAt: reentryAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatal(err)
	}
	f := unpublishedReentryFixture{st: st, runID: runID}
	f.prior = putReentryReview(t, st, runID, 1, reentryBase1, reentryHead1)
	f.audit(t, "owner/repo", reentryBase1)
	if slotKind != "" {
		f.reserve(t, domain.ProductionPublicationInvocationID(runID), slotKind)
	}
	return f
}

// audit records a workflow audit of repo that observed tip.
func (f unpublishedReentryFixture) audit(t *testing.T, repo, tip string) {
	t.Helper()
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo + `","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.WriteInternal(context.Background(), func(tx *InternalTx) error {
		_, err := tx.RecordWorkflowAudit(context.Background(), domain.WorkflowAudit{
			Repo: repo, AuditedCommitSHA: tip, AuditedAt: reentryAt,
			WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// reserve puts publication's intent slot in the given kind: a bare
// reservation, or a committed intent.
func (f unpublishedReentryFixture) reserve(t *testing.T, publication domain.InvocationID, kind string) {
	t.Helper()
	key, err := publicationrecord.IntentKey(publication, readyPublicationIntentKind)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.WriteInternal(context.Background(), func(tx *InternalTx) error {
		_, _, err := tx.EnqueueOutbox(context.Background(), key, kind, []byte(`{}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// authority is a v5 successor of predecessor that re-enters after prior
// toward base.
func (f unpublishedReentryFixture) authority(
	predecessor domain.ItemID, prior domain.ReviewRecord, base string,
) domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationUnpublishedReentryVersion, Origin: domain.PublicationSuccessorUnpublishedBaseAdvance,
		RunID: f.runID, PredecessorItemID: predecessor, PriorReviewInvocationID: prior.InvocationID,
		ReviewRound: prior.Round + 1,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: base, HeadSHA: prior.HeadSHA,
		},
	}
}

func (f unpublishedReentryFixture) record(authority domain.PublicationSuccessor) error {
	ctx := context.Background()
	return f.st.Write(ctx, func(tx *WriteTx) error { return tx.RecordPublicationSuccessor(ctx, authority) })
}

func (f unpublishedReentryFixture) validate(authority domain.PublicationSuccessor) error {
	ctx := context.Background()
	return f.st.Read(ctx, func(tx *ReadTx) error { return tx.validatePublicationSuccessor(ctx, authority) })
}

// storeLegacy writes authority as a daemon before issue #2644 sealed it,
// past the seal that now refuses a new v5 successor, so the read gates are
// exercised against stored history.
func (f unpublishedReentryFixture) storeLegacy(t *testing.T, authority domain.PublicationSuccessor) {
	t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, authority.Key(), domain.PublicationSuccessorKind, body); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, authority.Key())
	}); err != nil {
		t.Fatal(err)
	}
}

func (f unpublishedReentryFixture) current(t *testing.T) *domain.PublicationSuccessor {
	t.Helper()
	ctx := context.Background()
	var current *domain.PublicationSuccessor
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		var err error
		current, err = tx.CurrentPublicationSuccessor(ctx, f.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return current
}

// No cycle re-enters an unpublished head after issue #2644: the seal refuses
// a new v5 successor and records nothing, while one an earlier daemon sealed
// still reads as the run's current successor.
func TestUnpublishedReentrySealIsRetired(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.record(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("seal of a v5 re-entry = %v, want ErrParentKeyMismatch", err)
	}
	if current := f.current(t); current != nil {
		t.Fatalf("refused seal recorded successor %s", current.PublicationID())
	}
	f.storeLegacy(t, authority)
	if current := f.current(t); current == nil || current.PublicationID() != authority.PublicationID() {
		t.Fatalf("current successor = %+v, want stored %s", current, authority.PublicationID())
	}
}

// The every-read gate accepts a tip any recorded audit of the run's
// repository observed, not only the latest.
func TestUnpublishedReentryReadsAnOlderAuditTip(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	f.audit(t, "owner/repo", reentryBase3)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.validate(authority); err != nil {
		t.Fatalf("every-read gate refused a recorded audit tip: %v", err)
	}
}

func TestUnpublishedReentryRefusesAnUnauditedBase(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	// Another repository's audit observed the base: it proves nothing for
	// this run, whose admissions name owner/repo.
	f.audit(t, "other/repo", reentryBase2)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.validate(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("every-read gate on an unaudited base = %v, want ErrParentKeyMismatch", err)
	}
}

// A stored chain of v5 re-entries still reads: its tip is the run's current
// successor.
func TestStoredUnpublishedReentryChainReads(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	first := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	f.storeLegacy(t, first)
	second := putReentryReview(t, f.st, f.runID, 2, reentryBase2, reentryHead1)
	f.audit(t, "owner/repo", reentryBase3)
	tip := f.authority(first.ReadyItemID(), second, reentryBase3)
	f.storeLegacy(t, tip)
	if current := f.current(t); current == nil || current.PublicationID() != tip.PublicationID() {
		t.Fatalf("current successor = %+v, want chain tip %s", current, tip.PublicationID())
	}
}

func TestLatestRunWorkflowAuditTip(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	f.audit(t, "other/repo", reentryBase3)
	ctx := context.Background()
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		tip, err := tx.LatestRunWorkflowAuditTip(ctx, f.runID)
		if err == nil && tip != reentryBase2 {
			t.Fatalf("latest tip = %s, want %s from the run's own repository", tip, reentryBase2)
		}
		if err != nil {
			return err
		}
		// With no admission there is no repository to read audits of, so
		// the read fails closed rather than report no tip.
		if _, err := tx.LatestRunWorkflowAuditTip(ctx, "run-without-admissions"); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("tip of a run with no admissions = %v, want ErrParentKeyMismatch", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
