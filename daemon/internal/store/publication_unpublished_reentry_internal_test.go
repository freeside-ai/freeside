package store

import (
	"context"
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

func TestUnpublishedReentrySealsAtTheLatestAuditTip(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.record(authority); err != nil {
		t.Fatalf("seal at the latest audit tip: %v", err)
	}
	ctx := context.Background()
	if err := f.st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.CurrentPublicationSuccessor(ctx, f.runID)
		if err == nil && (got == nil || got.PublicationID() != authority.PublicationID()) {
			t.Fatalf("current successor = %+v, want %s", got, authority.PublicationID())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// The seal takes its base only from the daemon's latest audit: a tip an
// older audit observed is history the every-read gate accepts, never a base
// a new cycle may start toward.
func TestUnpublishedReentryRefusesABaseThatIsNotTheLatestAuditTip(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	f.audit(t, "owner/repo", reentryBase3)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.validate(authority); err != nil {
		t.Fatalf("every-read gate refused a recorded audit tip: %v", err)
	}
	if err := f.record(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("seal at an older audit tip = %v, want ErrParentKeyMismatch", err)
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
	if err := f.record(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("seal at an unaudited base = %v, want ErrParentKeyMismatch", err)
	}
}

func TestUnpublishedReentryRefusesTheReviewedBase(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase1)
	if err := f.record(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("seal at the base the prior round reviewed = %v, want ErrParentKeyMismatch", err)
	}
}

// An intent that committed may already have reached the forge; recovery
// converges it, and no re-entry may replace it with a second publication.
func TestUnpublishedReentryRefusesACommittedIntent(t *testing.T) {
	for name, kind := range map[string]string{
		"committed intent": readyPublicationIntentKind,
		"no reservation":   "",
	} {
		t.Run(name, func(t *testing.T) {
			f := seedUnpublishedReentry(t, kind)
			f.audit(t, "owner/repo", reentryBase2)
			authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
			if err := f.record(authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("seal over %s = %v, want ErrParentKeyMismatch", name, err)
			}
		})
	}
}

// A second advance extends the chain from its tip, replacing the first
// re-entry's publication. A second successor of the root would branch the
// chain; its identity is the first's, so the seal refuses it as a rewrite.
func TestUnpublishedReentryExtendsOnlyTheChainTip(t *testing.T) {
	f := seedUnpublishedReentry(t, publicationReservationKind)
	f.audit(t, "owner/repo", reentryBase2)
	first := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.record(first); err != nil {
		t.Fatal(err)
	}
	second := putReentryReview(t, f.st, f.runID, 2, reentryBase2, reentryHead1)
	f.audit(t, "owner/repo", reentryBase3)
	f.reserve(t, first.PublicationID(), publicationReservationKind)

	branch := f.authority(domain.ProductionReadyItemID(f.runID), second, reentryBase3)
	if err := f.record(branch); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("seal from the root after a sealed re-entry = %v, want ErrImmutableTransition", err)
	}
	if err := f.record(f.authority(first.ReadyItemID(), second, reentryBase3)); err != nil {
		t.Fatalf("seal from the chain tip: %v", err)
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
