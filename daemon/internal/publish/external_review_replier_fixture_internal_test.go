package publish

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// Seed a real publication through the public store gates. This fixture follows
// the store's ready-binding fixture so recovery tests never bypass authority.
func putReplyFixtureItem(ctx context.Context, tx *store.WriteTx, item *domain.AttentionItem) error {
	if err := storetest.BindSubject(ctx, tx, item); err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, *item)
}

type replyBindingFixture struct {
	path           string
	st             *store.Store
	run            domain.Run
	item           domain.AttentionItem
	admission      domain.ExecutionAdmission
	export         domain.ExecutionExport
	binding        domain.ReadyItemPRBinding
	intentKey      string
	intentPayload  []byte
	outcomePayload []byte
}

// readyAnchorSpecDigest is a well-formed content address, so an artifact
// that must restate the run's spec digest (a finding adjudication) can be
// written against this fixture.
var readyAnchorSpecDigest = domain.Digest("sha256:" + strings.Repeat("5", 64))

func seedReplyBinding(t *testing.T, branch, baseSHA, headSHA string) replyBindingFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}})
	f := seedReplyBindingIn(t, st, replyBindingSeed{"run-ready-anchor", "owner/repo", 424242, 450}, branch, baseSHA, headSHA)
	f.path = path
	return f
}

type replyBindingSeed struct {
	runID        domain.RunID
	repo         string
	repositoryID int64
	prNumber     int
}

func seedReplyBindingIn(t *testing.T, st *store.Store, seed replyBindingSeed, branch, baseSHA, headSHA string) replyBindingFixture {
	t.Helper()
	ctx := context.Background()
	runID := seed.runID
	invocationID := domain.InvocationID("inv-" + string(runID))
	stageID := domain.StageID("stage-" + string(runID))
	attemptID := domain.AttemptID("attempt-" + string(runID))
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: domain.ProjectID("project-" + string(runID)), SpecDigest: readyAnchorSpecDigest, PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: headSHA, PRReference: &domain.PRReference{Repo: seed.repo, Number: seed.prNumber},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return putReplyFixtureItem(ctx, tx, &item)
	}); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "ready-anchor-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: seed.repo, RepositoryID: seed.repositoryID, BaseRef: "main", BaseSHA: baseSHA},
		Workspace: "workspace", AdmittedAt: admittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: item.PRHeadSHA,
		ManifestDigest: "sha256:manifest", RecordedAt: admittedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest(contentaddr.Sum([]byte("reply-publication/" + string(runID))))
	publicationInvocationID := domain.InvocationID("publish-production-" + string(runID))
	intentPayload, err := json.Marshal(publicationrecord.Intent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      identity, InvocationID: publicationInvocationID,
		Branch: branch,
		Repo:   admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		SourceHeadSHA:         export.HeadSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		ProducingInvocationID: invocationID, ReservationRunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(publicationrecord.Outcome{
		Identity: identity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		HeadSHA: export.HeadSHA, Branch: branch,
		PRNumber: seed.prNumber, EvidenceEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.ReadyItemPRBinding{
		ItemID: item.ID, RunID: runID, ProducingInvocationID: invocationID,
		PublicationInvocationID: publicationInvocationID,
		PublicationIdentity:     identity, Repo: admission.Base.Repo,
		RepositoryID: admission.Base.RepositoryID, PRNumber: seed.prNumber,
		BaseRef: admission.Base.BaseRef, HeadSHA: export.HeadSHA,
		RecordedAt: admittedAt.Add(2 * time.Minute),
	}
	intentKey := "publish/" + string(publicationInvocationID) + "/" + IntentKindPublication
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, IntentKindPublication, intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(identity), "publish.outcome", payload); err != nil {
			return err
		}
		return tx.RecordReadyItemPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	return replyBindingFixture{
		st: st, run: run, item: item, admission: admission, export: export, binding: binding,
		intentKey: intentKey, intentPayload: intentPayload, outcomePayload: payload,
	}
}

func (f *replyBindingFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = storetest.Open(t, f.path, store.Options{AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}})
}
