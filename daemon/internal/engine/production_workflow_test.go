package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func productionEntry(payload string) store.QueueEntry {
	return store.QueueEntry{
		IdempotencyKey: "inv-implement-run-1",
		Kind:           KindProductionInvocationRequested,
		Payload:        []byte(payload),
	}
}

func TestProductionDeliveryRefusalRecordsStandardOutcome(t *testing.T) {
	ctx := t.Context()
	runID := domain.RunID("run-delivery-refusal")
	invocationID := remediationInvocationID(runID, 1)
	stageID := remediationStageID(runID, 1)
	identity := domain.AuthIdentity{
		ID: "auth-delivery-refusal", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand,
		},
	}
	identityID := identity.ID
	run := domain.Run{
		ID: runID, ProjectID: "project-delivery-refusal",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: productionStageName,
			Attempts: []domain.Attempt{{
				ID: attemptIDFor(invocationID), StageID: stageID,
				Number: 1, InvocationID: invocationID,
			}},
		}},
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID,
		AttemptID:      attemptIDFor(invocationID),
		Backend:        string(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 42,
			BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace: "ws-delivery-refusal", AuthIdentityID: &identityID,
		AdmittedAt: time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "delivery-refusal.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, identity, admission.AdmittedAt); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(
			ctx, string(invocationID), KindRemediationInvocationRequested, []byte(`{}`),
		); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: st, signet: signet.NewService(st)}
	if err := e.recordProductionDeliveryRefusal(
		ctx, run, run.Stages[0], invocationID, "rendered prompt exceeds limit",
	); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		outcome, err := tx.GetExecutionOutcomeRecord(ctx, invocationID)
		if err != nil {
			return err
		}
		if outcome.Status != domain.ExecutionOutcomeFailed || outcome.AdmissionID != admission.ID {
			return fmt.Errorf("delivery refusal outcome = %#v", outcome)
		}
		active, err := tx.ActiveIdentityExecutionCount(ctx, identity.ID)
		if err != nil {
			return err
		}
		if active != 0 {
			return fmt.Errorf("active executions after terminal refusal = %d", active)
		}
		marker, err := tx.GetOutbox(ctx, string(invocationID))
		if err != nil {
			return err
		}
		if !marker.Dispatched() {
			return errors.New("delivery refusal marker remained pending")
		}
		_, err = tx.GetAttentionItem(ctx, domain.ItemID("execution-failure-"+string(invocationID)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReadyItemPreservesReadinessSummary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	w := &productionPublicationWorkflow{now: func() time.Time { return now }}
	task := productionPublicationTask{
		RunID: "run-ready", ProjectID: "project-ready", ProducingInvocationID: "inv-ready",
	}
	summary := summaryClaimFixture(task.ProducingInvocationID, "The candidate is ready for review.")
	checkpoint := productionVerificationCheckpoint{
		Imported: importer.Result{
			CommitSHA: strings.Repeat("a", 40), Claims: []domain.AgentClaim{summary},
		},
		Authorization: domain.CandidateAuthorization{
			Repo: "owner/repo",
		},
	}
	published := publish.Result{PRNumber: 123}
	yieldHistory := domain.ReviewYieldHistory{
		Rounds:          []domain.ReviewYieldRound{{Round: 1, Outcome: domain.ReviewClean}},
		TerminalOutcome: domain.ReviewClean,
	}

	recipe := domain.Digest("sha256:recipe")
	detailFor := func(verdict domain.ReadinessVerdict) domain.ReadinessDetail {
		detail := domain.ReadinessDetail{
			EvaluationSetDigest: verdict.EvaluationSetDigest,
			CandidateHead:       checkpoint.Imported.CommitSHA,
			Base:                domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: strings.Repeat("b", 40)},
			Requirements: []domain.ReadinessRequirement{{
				RequirementKey: "clean-verification", CheckClass: domain.CheckClassCleanVerification,
				Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed,
				ProofRecipeDigest: &recipe,
			}},
		}
		if verdict.Class == domain.ReadinessReadyDegraded {
			detail.Requirements = append(detail.Requirements, domain.ReadinessRequirement{
				RequirementKey: "optional-check", CheckClass: domain.CheckClassRepoChangePolicy,
				Kind: domain.RequirementOptional, State: domain.ReadinessRequirementFailed,
			})
		}
		return detail
	}
	for _, verdict := range []domain.ReadinessVerdict{
		{Class: domain.ReadinessReadyClean, EvaluationSetDigest: "sha256:evaluation-clean"},
		{
			Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation-degraded",
			AdvisoryOutcomes: []domain.AdvisoryOutcomeRecord{{
				RequirementResolutionDigest: "sha256:optional-check",
				Outcome:                     domain.AdvisoryFailed,
			}},
		},
	} {
		t.Run(string(verdict.Class), func(t *testing.T) {
			t.Parallel()
			if err := verdict.Validate(); err != nil {
				t.Fatalf("test verdict: %v", err)
			}
			readiness := productionReadiness{verdict: verdict, detail: detailFor(verdict)}
			item, err := w.readyItem(t.Context(), task, checkpoint, published, readiness, yieldHistory)
			if err != nil {
				t.Fatalf("readyItem: %v", err)
			}
			if item.Readiness == nil || item.Readiness.Class != verdict.Class ||
				item.Readiness.EvaluationSetDigest != verdict.EvaluationSetDigest {
				t.Fatalf("ready item summary = %+v, want %q / %q",
					item.Readiness, verdict.Class, verdict.EvaluationSetDigest)
			}
			if item.ReadinessDetail == nil || !reflect.DeepEqual(*item.ReadinessDetail, readiness.detail) {
				t.Fatalf("ready item detail = %+v, want %+v", item.ReadinessDetail, readiness.detail)
			}
			if item.YieldHistory == nil || !reflect.DeepEqual(*item.YieldHistory, yieldHistory) {
				t.Fatalf("ready item yield history = %+v, want %+v", item.YieldHistory, yieldHistory)
			}
			if len(item.AgentClaims) != 1 || item.AgentClaims[0].Text == nil ||
				item.AgentClaims[0].Provenance.ProducerInvocationID != task.ProducingInvocationID {
				t.Fatalf("ready item summary claim = %+v", item.AgentClaims)
			}
		})
	}
}

func TestProductionBlockedItemCarriesInvocationBoundSummary(t *testing.T) {
	t.Parallel()
	task := productionPublicationTask{
		RunID: "run-blocked", ProjectID: "project-blocked", ProducingInvocationID: "inv-blocked",
	}
	summary := summaryClaimFixture(task.ProducingInvocationID, "Publication stopped at the trust gate.")
	w := &productionPublicationWorkflow{now: func() time.Time { return time.Unix(1, 0).UTC() }}
	item, err := w.blockedItem(t.Context(), task, importer.Result{
		CommitSHA: strings.Repeat("b", 40), Claims: []domain.AgentClaim{summary},
	}, nil, "Trust evaluation failed.")
	if err != nil {
		t.Fatalf("blockedItem: %v", err)
	}
	if len(item.AgentClaims) != 1 || item.AgentClaims[0].Text == nil ||
		item.AgentClaims[0].Label != export.SummaryEvidenceLabel {
		t.Fatalf("blocked item summary claim = %+v", item.AgentClaims)
	}
}

func TestProductionTerminalFailureWritesAdvisoryDiagnosticOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "store.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := func() time.Time { return time.Unix(2, 0).UTC() }
	advisoryStore, err := advisory.Open(
		filepath.Join(dir, "advisory.json"), 20, 16<<10, advisory.WithClock(now),
	)
	if err != nil {
		t.Fatal(err)
	}
	driver := inferencefake.New()
	driver.Script(inference.DiagnosticSiteID, inferencefake.Script{Response: inference.Response{
		Output:       []byte(`{"probable_cause":"tool failure","explanation":"the stage returned failed"}`),
		ComputeUnits: 2,
	}})
	limits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	site := inference.DiagnosticSite(inference.Budget{
		Window: time.Hour, Site: limits, Project: limits, Global: limits,
		MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
	})
	site.AuditEvery = 1
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(dir, "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "diagnostic", Driver: driver},
		Sites:     []inference.Site{site}, Advisory: advisoryStore,
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow := &Engine{store: st, inference: client}
	run := domain.Run{ID: "run-1", ProjectID: "project-1"}
	terminal := productionTerminalRecord{
		InvocationID: "inv-1", RunID: run.ID, StageID: "stage-1",
		Status: exec.StatusFailed, Summary: "exit 1",
	}
	if completed, err := workflow.recordProductionTerminalWithAuthority(ctx, run, terminal, false); err != nil || completed {
		t.Fatalf("record failure = %v, %v", completed, err)
	}
	if completed, err := workflow.recordProductionTerminalWithAuthority(ctx, run, terminal, false); err != nil || completed {
		t.Fatalf("replay failure = %v, %v", completed, err)
	}
	entries, err := advisoryStore.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Kind != "diagnostic_claim" {
		t.Fatalf("advisory entries = %#v", entries)
	}
}

type transientCleanupReviewSource struct {
	cause        error
	inspectID    domain.InvocationID
	requestCalls int
}

func (s *transientCleanupReviewSource) RequestReview(
	context.Context, domain.InvocationID, exec.ReviewRequest,
) error {
	s.requestCalls++
	return errors.New("unexpected review request")
}

func (s *transientCleanupReviewSource) Inspect(
	_ context.Context, id domain.InvocationID,
) (exec.Status, error) {
	s.inspectID = id
	return "", &exec.ReviewSourceFailure{Class: domain.ReviewFailureTransient, Err: s.cause}
}

func (*transientCleanupReviewSource) Poll(
	context.Context, domain.InvocationID,
) (exec.ReviewResult, error) {
	return exec.ReviewResult{}, errors.New("unexpected review poll")
}

func (*transientCleanupReviewSource) Verify(
	context.Context, domain.InvocationID, string, string,
) error {
	return errors.New("unexpected review verification")
}

func (*transientCleanupReviewSource) VerifyRequestAuthority(
	context.Context, domain.InvocationID, domain.Digest,
) error {
	return nil
}

func TestNormalizeTerminalReviewFailurePreservesDeclaredClass(t *testing.T) {
	for _, class := range []domain.ReviewFailureClass{
		domain.ReviewFailureTransient,
		domain.ReviewFailureConfiguration,
		domain.ReviewFailureQuota,
		domain.ReviewFailureContradiction,
	} {
		t.Run(string(class), func(t *testing.T) {
			declared := &exec.ReviewSourceFailure{
				Class: class, Err: errors.New("terminal source failure"),
			}
			err := normalizeTerminalReviewFailure(errors.Join(exec.ErrNoResult, declared))
			if exec.ClassifyReviewSourceFailure(err) != class || !errors.Is(err, exec.ErrNoResult) {
				t.Fatalf("normalized %s failure = %v", class, err)
			}
		})
	}
	bare := normalizeTerminalReviewFailure(exec.ErrNoResult)
	if exec.ClassifyReviewSourceFailure(bare) != domain.ReviewFailureTransient ||
		!errors.Is(bare, exec.ErrNoResult) {
		t.Fatalf("normalized bare no-result = %v", bare)
	}
}

func TestReviewConfigurationUnapprovedErrorNamesTheConfigurationMismatch(t *testing.T) {
	pinned := domain.Digest("sha256:profile-pinned")
	effective := domain.Digest("sha256:daemon-effective")
	err := reviewConfigurationUnapprovedError(pinned, effective)

	if !errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
		t.Fatalf("configuration mismatch = %v, want %v", err, domain.ErrReviewConfigurationUnapproved)
	}
	if errors.Is(err, domain.ErrTrustProfileSuperseded) {
		t.Fatalf("configuration mismatch misclassified as profile supersession: %v", err)
	}
	want := "profile pins sha256:profile-pinned, daemon effective is sha256:daemon-effective: " +
		domain.ErrReviewConfigurationUnapproved.Error()
	if err.Error() != want {
		t.Fatalf("configuration mismatch reason = %q, want %q", err, want)
	}
}

func TestProductionReviewTransientSourceFailureSchedulesSameInvocationRetry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{
		ID: "run-review-artifact-retry", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	w := &productionPublicationWorkflow{
		store: st, now: func() time.Time { return now },
		reviewRetryAfter: make(map[domain.RunID]time.Time),
	}
	id := ProductionReviewInvocationID(run.ID, 1)
	cause := &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureTransient,
		Err:   errors.New("read persisted review instruction artifact: input/output error"),
	}
	state, err := w.retryOrRecordReviewFailure(
		ctx, productionPublicationTask{RunID: run.ID}, id, 1, "base", "head", cause,
	)
	if err != nil || state != productionReviewPending {
		t.Fatalf("retry routing = %q, %v", state, err)
	}
	if got := w.reviewRetryAfter[run.ID]; !got.Equal(now.Add(reviewRetryDelay(1))) {
		t.Fatalf("in-memory retry deadline = %v", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		retry, err := tx.GetReviewRetry(ctx, run.ID)
		if err != nil {
			return err
		}
		if retry.InvocationID != id || retry.Round != 1 || retry.BaseSHA != "base" ||
			retry.HeadSHA != "head" || retry.ObservedAt != now || !strings.Contains(retry.Reason, "input/output error") {
			t.Fatalf("durable retry = %#v", retry)
		}
		if _, err := tx.GetReviewFailure(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("transient materialization recorded terminal failure: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionReviewTransientCleanupFailureSchedulesSameInvocationRetry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runID := domain.RunID("run-review-cleanup-retry")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: "25",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: "sha256:review-policy",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	cleanupErr := errors.New("runtime cleanup temporarily unavailable")
	source := &transientCleanupReviewSource{cause: cleanupErr}
	configDigest := domain.Digest("sha256:" + strings.Repeat("c", 64))
	w := &productionPublicationWorkflow{
		store: st, now: func() time.Time { return now }, workDir: t.TempDir(),
		reviewSource: source, reviewConfigurationDigest: configDigest,
		reviewRetryAfter: make(map[domain.RunID]time.Time),
	}
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	state, err := w.reconcileReviewGate(
		ctx,
		productionPublicationTask{RunID: runID, HeadSHA: headSHA},
		productionBinding{
			admission: domain.ExecutionAdmission{Base: domain.BaseRevision{
				Repo: "owner/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: baseSHA,
			}},
			profile: domain.AutomationTrustProfile{Review: domain.ReviewSettings{
				Mode: domain.ReviewFreesideInvoked, ConfigDigest: configDigest,
			}},
		},
		productionVerificationCheckpoint{Authorization: domain.CandidateAuthorization{
			VerificationRecipeDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
			EvidenceSnapshotDigest:   domain.Digest("sha256:" + strings.Repeat("e", 64)),
			VerificationOutcome:      domain.VerificationPassed,
		}},
		nil,
		instructions,
	)
	if err != nil || state != productionReviewPending {
		t.Fatalf("cleanup retry routing = %q, %v", state, err)
	}
	id := ProductionReviewInvocationID(runID, 1)
	if source.inspectID != id || source.requestCalls != 0 {
		t.Fatalf("cleanup retry invocation = %q, requests=%d; want %q, 0",
			source.inspectID, source.requestCalls, id)
	}
	if got := w.reviewRetryAfter[runID]; !got.Equal(now.Add(reviewRetryDelay(1))) {
		t.Fatalf("in-memory cleanup retry deadline = %v", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		retry, err := tx.GetReviewRetry(ctx, runID)
		if err != nil {
			return err
		}
		if retry.InvocationID != id || retry.Round != 1 || retry.BaseSHA != baseSHA ||
			retry.HeadSHA != headSHA || retry.ObservedAt != now || !strings.Contains(retry.Reason, cleanupErr.Error()) {
			t.Fatalf("durable cleanup retry = %#v", retry)
		}
		if _, err := tx.GetReviewFailure(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("transient cleanup recorded terminal failure: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionHoldRetryPruning(t *testing.T) {
	pendingKey := productionPublicationTaskKey("run-pending")
	finishedKey := productionPublicationTaskKey("run-finished")
	w := productionPublicationWorkflow{holdRetryAfter: map[string]time.Time{
		pendingKey:  time.Unix(1, 0),
		finishedKey: time.Unix(2, 0),
	}}
	w.pruneHeldTaskRetries([]store.QueueEntry{{
		IdempotencyKey: pendingKey,
	}})
	if _, found := w.holdRetryAfter[pendingKey]; !found {
		t.Fatal("pending task retry deadline was pruned")
	}
	if _, found := w.holdRetryAfter[finishedKey]; found {
		t.Fatal("finished task retry deadline was retained")
	}
}

func TestProductionPublicationErrorClassification(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		domain.ErrParentKeyMismatch,
		domain.ErrImmutableTransition,
		domain.ErrInvalidOperatingMode,
		domain.ErrPathBoundaryMismatch,
		store.ErrNotFound,
		store.ErrImmutableConflict,
		store.ErrStaleWrite,
	} {
		if !productionPublicationStateContradiction(fmt.Errorf("reconcile task: %w", err)) {
			t.Errorf("%v was not classified as a durable contradiction", err)
		}
		if productionPublicationRetryableFailure(fmt.Errorf("reconcile task: %w", err)) {
			t.Errorf("%v was classified as retryable", err)
		}
	}
	for _, err := range []error{
		productionPublicationRetryableError(errors.New("container exited -1")),
		&net.DNSError{Err: "temporary", Name: "github.com"},
		&publish.APIError{Status: 503, RequestPath: "/repos/o/r"},
		&publish.TransportGitError{
			Args: []string{"fetch"}, ExitCode: -1,
			Refusal: publish.RefusalUnknown, Err: context.DeadlineExceeded,
		},
		publish.ErrJanitorInactive,
		publish.ErrInstallationGrantUntrusted,
		&os.PathError{Op: "write", Path: "/state", Err: os.ErrPermission},
		context.DeadlineExceeded,
	} {
		if !productionPublicationRetryableFailure(err) {
			t.Errorf("%v was not classified as retryable", err)
		}
	}
	for _, err := range []error{
		errors.New("malformed durable checkpoint"),
		publish.ErrGitTransport,
	} {
		if productionPublicationRetryableFailure(err) {
			t.Errorf("durable or untyped error %v was classified as retryable", err)
		}
	}
	if !productionPublicationPermanentExternalFailure(&publish.APIError{Status: 401, RequestPath: "/repos/o/r"}) {
		t.Fatal("a permanent forge refusal was not classified for durable hold")
	}
	for _, status := range []int{403, 429, 503} {
		if productionPublicationPermanentExternalFailure(&publish.APIError{
			Status: status, RequestPath: "/repos/o/r",
		}) {
			t.Errorf("retryable forge status %d was classified for durable hold", status)
		}
	}
	for _, err := range []error{
		publish.ErrRemoteMissingBase,
		publish.ErrAmbiguousInstallation,
		publish.ErrInstallationResolution,
		publish.ErrGrantMismatch,
	} {
		if !productionPublicationPermanentExternalFailure(err) {
			t.Errorf("permanent external refusal %v was not classified for durable hold", err)
		}
	}
	if !productionPublicationPermanentExternalFailure(&publish.TransportGitError{
		Args: []string{"push"}, ExitCode: 128, Refusal: publish.RefusalAuth,
	}) {
		t.Fatal("a permanent transport authentication refusal was not classified for durable hold")
	}
}

const testProductionPublicationJSON = `"publication":{"title":"Test production work item","body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`

func productionRequestJSON(fields string) string {
	return `{` + fields + `,` + testProductionPublicationJSON + `}`
}

func TestDecodeProductionRequestRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()
	canonical := productionRequestJSON(
		`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`,
	)
	tests := []struct {
		name    string
		entry   store.QueueEntry
		wantErr error
	}{
		{"empty", productionEntry(``), nil},
		{"trailing value", productionEntry(canonical + ` {}`), nil},
		{"unknown field", productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1","extra":1`)), nil},
		{"noncanonical legacy", productionEntry(`{ "invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`), domain.ErrParentKeyMismatch},
		{"null version", productionEntry(`{"version":null,"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`), nil},
		{"unknown version", productionEntry(productionRequestJSON(`"version":"freeside.production-invocation/v3","invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`)), nil},
		{"v2 missing publication", productionEntry(`{"version":"freeside.production-invocation/v2","invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`), nil},
		{"null publication", productionEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1","publication":null}`), nil},
		{"missing run", productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","stage_id":"implement-run-1"`)), domain.ErrEmptyID},
		{"missing stage", productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1"`)), domain.ErrEmptyID},
		{"key mismatch", func() store.QueueEntry {
			e := productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-2","run_id":"run-2","stage_id":"implement-run-2"`))
			return e
		}(), domain.ErrParentKeyMismatch},
		{"foreign kind", func() store.QueueEntry {
			e := productionEntry(canonical)
			e.Kind = "agent_invocation_requested"
			return e
		}(), domain.ErrParentKeyMismatch},
		{"underived invocation id", func() store.QueueEntry {
			e := productionEntry(productionRequestJSON(`"invocation_id":"inv-custom","run_id":"run-1","stage_id":"implement-run-1"`))
			e.IdempotencyKey = "inv-custom"
			return e
		}(), domain.ErrParentKeyMismatch},
		{"underived stage id", func() store.QueueEntry {
			e := productionEntry(productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"feedback-run-1"`))
			return e
		}(), domain.ErrParentKeyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeProductionRequest(tc.entry)
			if err == nil {
				t.Fatal("decode accepted malformed entry")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func terminalEntry(payload string) store.QueueEntry {
	return store.QueueEntry{
		IdempotencyKey: "inv-implement-run-1",
		Kind:           kindProductionStageTerminal,
		Payload:        []byte(payload),
	}
}

// TestDecodeProductionTerminalRejectsForgedRecords: a stored terminal record
// is a reconstruction boundary. Trusting one by kind alone lets a corrupted
// or fabricated row permanently suppress an attempt's collection, which
// means neither an accepted result nor the execution_failure item that would
// otherwise make the failure visible.
func TestDecodeProductionTerminalRejectsForgedRecords(t *testing.T) {
	t.Parallel()
	run := domain.Run{ID: "run-1", ProjectID: "proj-1"}
	canonical := `{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
		`"stage_id":"implement-run-1","status":"completed"}`
	tests := []struct {
		name    string
		entry   store.QueueEntry
		wantErr error
	}{
		{"empty", terminalEntry(``), nil},
		{"trailing value", terminalEntry(canonical + ` {}`), nil},
		{"unknown field", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"completed","extra":1}`), nil},
		{"foreign run", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-2",` +
			`"stage_id":"implement-run-2","status":"completed"}`), domain.ErrParentKeyMismatch},
		{"underived stage", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"feedback-run-1","status":"completed"}`), domain.ErrParentKeyMismatch},
		{"non-terminal status", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"running"}`), exec.ErrInvalidStatus},
		{"unknown status", terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"finished"}`), exec.ErrInvalidStatus},
		{"foreign kind", func() store.QueueEntry {
			e := terminalEntry(canonical)
			e.Kind = "agent_completion"
			return e
		}(), domain.ErrParentKeyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeProductionTerminal(tc.entry, run)
			if err == nil {
				t.Fatal("a forged terminal record was accepted as authoritative")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// The lane's own records still round-trip, including the gone outcome it
	// writes for a session lost without a result.
	for _, status := range []exec.Status{exec.StatusCompleted, exec.StatusFailed, exec.StatusCanceled, exec.StatusGone} {
		entry := terminalEntry(`{"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
			`"stage_id":"implement-run-1","status":"` + string(status) + `"}`)
		if _, err := decodeProductionTerminal(entry, run); err != nil {
			t.Errorf("canonical %q record rejected: %v", status, err)
		}
	}
}

func TestDecodeProductionRequestAcceptsCanonicalPayload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		payload     string
		wantLegacy  bool
		wantVersion string
	}{
		{
			name:       "released legacy v1",
			payload:    `{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`,
			wantLegacy: true,
		},
		{
			name:    "unversioned publication preview",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`),
		},
		{
			name:        "publication v2",
			payload:     productionRequestJSON(`"version":"freeside.production-invocation/v2","invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`),
			wantVersion: productionInvocationRequestVersion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeProductionRequest(productionEntry(tc.payload))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.InvocationID != "inv-implement-run-1" || got.RunID != "run-1" ||
				got.StageID != "implement-run-1" || got.Legacy != tc.wantLegacy || got.Version != tc.wantVersion {
				t.Fatalf("decoded request = %#v", got)
			}
		})
	}
}

func TestProductionOwnershipReGatesTheMarkerPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name       string
		runID      domain.RunID
		payload    string
		wantReason string
	}{
		{
			name:  "canonical",
			runID: "run-owned",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-owned",` +
				`"run_id":"run-owned","stage_id":"implement-run-owned"`),
		},
		{
			name:       "malformed",
			runID:      "run-malformed",
			payload:    `{"run_id":"run-malformed"}`,
			wantReason: productionQuarantineUnreadable,
		},
		{
			name:  "retargeted",
			runID: "run-retargeted",
			payload: productionRequestJSON(`"invocation_id":"inv-implement-run-other",` +
				`"run_id":"run-other","stage_id":"implement-run-other"`),
			wantReason: productionQuarantineUnreadable,
		},
		{
			name:  "future version",
			runID: "run-future",
			payload: productionRequestJSON(`"version":"freeside.production-invocation/v3",` +
				`"invocation_id":"inv-implement-run-future",` +
				`"run_id":"run-future","stage_id":"implement-run-future"`),
			wantReason: productionQuarantineUnsupportedVersion,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, st := newQuarantineEngine(t, ctx)
			run := productionOwnershipRun(tc.runID)
			seedProductionOwnershipRun(t, ctx, st, run)
			seedProductionMarker(t, ctx, st, tc.runID, tc.payload)

			owned, err := e.ownsProductionRun(ctx, run)
			if err != nil {
				t.Fatalf("ownership = %v, %v", owned, err)
			}
			if tc.wantReason == "" {
				if !owned {
					t.Fatal("canonical marker did not report ownership")
				}
				requireNoQuarantineItem(t, ctx, st, tc.runID)
				return
			}
			if owned {
				t.Fatal("unauthentic marker reported ownership")
			}
			item := requireQuarantineItem(t, ctx, st, tc.runID)
			if item.Reason != tc.wantReason {
				t.Fatalf("quarantine reason = %q, want %q", item.Reason, tc.wantReason)
			}
			if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen ||
				item.Subject.ID != domain.SubjectID(tc.runID) || item.ProjectID != run.ProjectID {
				t.Fatalf("quarantine item = %#v", item)
			}
			if item.CreatedAt == nil {
				t.Fatal("quarantine item created_at is nil")
			}

			// A second pass (the restart case) converges on the one notice
			// instead of failing or duplicating it.
			owned, err = e.ownsProductionRun(ctx, run)
			if owned || err != nil {
				t.Fatalf("replayed ownership = %v, %v", owned, err)
			}
			requireQuarantineItem(t, ctx, st, tc.runID)
		})
	}
}

// TestProductionQuarantineLeavesADecidedNoticeAlone pins the property that
// makes the notice durable rather than merely repeated: a pass that finds the
// item already recorded writes nothing, so an operator's decision on it
// survives every later scan.
func TestProductionQuarantineLeavesADecidedNoticeAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-decided")
	run := productionOwnershipRun(runID)
	seedProductionOwnershipRun(t, ctx, st, run)
	seedProductionMarker(t, ctx, st, runID, `{"run_id":"run-decided"}`)

	if owned, err := e.ownsProductionRun(ctx, run); owned || err != nil {
		t.Fatalf("ownership = %v, %v", owned, err)
	}
	decided := requireQuarantineItem(t, ctx, st, runID)
	decided.Status = domain.StatusResolved
	decided.ItemVersion = 2
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, decided)
	}); err != nil {
		t.Fatalf("record operator decision: %v", err)
	}

	if owned, err := e.ownsProductionRun(ctx, run); owned || err != nil {
		t.Fatalf("ownership after decision = %v, %v", owned, err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.Status != domain.StatusResolved || current.ItemVersion != 2 {
		t.Fatalf("decided notice was rewritten: %#v", current)
	}
}

// TestProductionDispatchQuarantinesUnreadableMarkers covers the dispatch half
// of #424: a pending marker no daemon pass can decode leaves the loop instead
// of ending it, while a row this lane could not have filed stays loud because
// it names no run to quarantine.
func TestProductionDispatchQuarantinesUnreadableMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("attributable row is quarantined", func(t *testing.T) {
		e, st := newQuarantineEngine(t, ctx)
		runID := domain.RunID("run-pending-future")
		run := domain.Run{
			ID: runID, ProjectID: "project-1",
			SpecDigest:   domain.Digest("sha256:" + strings.Repeat("a", 64)),
			PolicyDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
			Stages: []domain.Stage{{
				ID: productionStageID(runID), RunID: runID,
				Name: productionStageName, Attempts: []domain.Attempt{},
			}},
		}
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutRun(ctx, run)
		}); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		seedProductionMarker(t, ctx, st, runID, productionRequestJSON(
			`"version":"freeside.production-invocation/v3",`+
				`"invocation_id":"inv-implement-run-pending-future",`+
				`"run_id":"run-pending-future","stage_id":"implement-run-pending-future"`,
		))

		started, err := e.dispatchPendingInvocations(ctx)
		if err != nil || started != 0 {
			t.Fatalf("dispatch = %d, %v", started, err)
		}
		item := requireQuarantineItem(t, ctx, st, runID)
		if item.Reason != productionQuarantineUnsupportedVersion {
			t.Fatalf("quarantine reason = %q", item.Reason)
		}
	})

	t.Run("unattributable row stays loud", func(t *testing.T) {
		e, st := newQuarantineEngine(t, ctx)
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(
				ctx, "not-a-production-key",
				KindProductionInvocationRequested, []byte(`{"run_id":"run-orphan"}`),
			)
			return err
		}); err != nil {
			t.Fatalf("seed marker: %v", err)
		}
		if _, err := e.dispatchPendingInvocations(ctx); err == nil {
			t.Fatal("unattributable production row dispatched without error")
		}
	})
}

// TestProductionQuarantineSkipsUnrelatedFailures keeps the classification
// narrow: only a marker reconstruction failure quarantines, so a store fault
// or any other cause still reaches the caller.
func TestProductionQuarantineSkipsUnrelatedFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	quarantined, err := quarantineProductionMarker(
		ctx, st, e.signet, "run-unrelated", "project-1", store.ErrNotFound)
	if quarantined || err != nil {
		t.Fatalf("quarantined unrelated cause = %v, %v", quarantined, err)
	}
	requireNoQuarantineItem(t, ctx, st, "run-unrelated")
}

func TestRemediationSourceOperationalStoreReadsRemainRetryable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var expired *store.ReadTx
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		expired = tx
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		read func() error
	}{
		{
			name: "checkpoint",
			read: func() error {
				_, err := expired.GetInbox(ctx, "checkpoint")
				return err
			},
		},
		{
			name: "authorization",
			read: func() error {
				_, err := expired.GetCandidateAuthorization(ctx, "sha256:authorization")
				return err
			},
		},
		{
			name: "image",
			read: func() error {
				_, err := expired.GetProjectImage(ctx, "sha256:image")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readErr := tc.read()
			if !errors.Is(readErr, sql.ErrTxDone) {
				t.Fatalf("%s store read = %v, want sql.ErrTxDone", tc.name, readErr)
			}
			classified := remediationSourceReadError(ctx, readErr)
			if errors.Is(classified, errRemediationSourceIdentity) ||
				!productionPublicationRetryableFailure(classified) {
				t.Fatalf("%s read = %v, want untagged retryable failure", tc.name, classified)
			}
		})
	}
}

func TestRemediationSourceSQLiteReadFailureRemainsRetryable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, dbPath, store.Options{BusyTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") })

	readErr := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(ctx, "checkpoint")
		return err
	})
	var sqliteErr *sqlite.Error
	if !errors.As(readErr, &sqliteErr) {
		t.Fatalf("locked store read = %v, want SQLite operational error", readErr)
	}
	classified := remediationSourceReadError(ctx, readErr)
	if errors.Is(classified, errRemediationSourceIdentity) ||
		!productionPublicationRetryableFailure(classified) {
		t.Fatalf("locked store read = %v, want untagged retryable failure", classified)
	}
}

// TestCheckpointArtifactVerifyRetryableUnlessDeterministic proves the #911
// re-review fix: a transient open/read/close fault while verifying a
// daemon-authored checkpoint blob stays retryable and untagged, while a digest
// mismatch terminalizes with the caller's durable sentinel. Both the remediation
// source-tree and durable production-checkpoint authenticators route their
// verifyFakePublicationBlob failures through retryableOrTerminal, so a transient
// I/O blip no longer converts an otherwise valid run into a permanent dispute.
func TestCheckpointArtifactVerifyRetryableUnlessDeterministic(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	artifact := domain.Artifact{
		ID: "artifact-1", Digest: domain.Digest("sha256:" + strings.Repeat("0", 64)),
		Metadata: runMeta(),
	}
	transientOpen := &os.PathError{Op: "open", Path: "checkpoint-blob", Err: errors.New("input/output error")}

	for _, sentinel := range []error{errRemediationSourceIdentity, domain.ErrParentKeyMismatch} {
		operational := verifyFakePublicationBlob(remediationArtifactStore{openErr: transientOpen}, artifact)
		if operational == nil {
			t.Fatal("expected an operational blob-open failure")
		}
		if classified := retryableOrTerminal(ctx, operational, sentinel); errors.Is(classified, sentinel) ||
			!productionPublicationRetryableFailure(classified) {
			t.Fatalf("transient open (sentinel %v) = %v, want untagged retryable", sentinel, classified)
		}

		deterministic := verifyFakePublicationBlob(remediationArtifactStore{body: []byte("mismatched content")}, artifact)
		if deterministic == nil {
			t.Fatal("expected a digest-mismatch failure")
		}
		if classified := retryableOrTerminal(ctx, deterministic, sentinel); !errors.Is(classified, sentinel) ||
			productionPublicationRetryableFailure(classified) {
			t.Fatalf("digest mismatch (sentinel %v) = %v, want terminal", sentinel, classified)
		}
	}

	// A canceled context is preserved, never terminalized as a contradiction.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	deterministic := verifyFakePublicationBlob(remediationArtifactStore{body: []byte("mismatched content")}, artifact)
	if got := retryableOrTerminal(canceled, deterministic, errRemediationSourceIdentity); !errors.Is(got, context.Canceled) ||
		errors.Is(got, errRemediationSourceIdentity) {
		t.Fatalf("canceled context = %v, want untagged context.Canceled", got)
	}
}

func TestRemediationSourceDurableStoreReadFailuresRemainTerminal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`INSERT INTO inbox (idempotency_key, kind, payload, status, created_at)
		 VALUES ('checkpoint', 'production_verification_checkpoint', X'7B7D', 'pending', 'not-a-time')`,
		`INSERT INTO candidate_authorizations
		 (id, repo, base_sha, head_sha, trust_profile_digest, created_at, body)
		 VALUES ('sha256:authorization', 'owner/repo', 'base', 'head', 'sha256:profile', 'now', 'not-json')`,
		`INSERT INTO project_images
		 (id, repository, repository_id, commit_sha, recipe_digest, base_image_ref, image_ref, body)
		 VALUES ('sha256:image', 'owner/repo', 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		 'sha256:recipe', 'base-image', 'image-ref', 'not-json')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed malformed source row: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		read func(*store.ReadTx) error
	}{
		{
			name: "checkpoint",
			read: func(tx *store.ReadTx) error {
				_, err := tx.GetInbox(ctx, "checkpoint")
				return err
			},
		},
		{
			name: "authorization",
			read: func(tx *store.ReadTx) error {
				_, err := tx.GetCandidateAuthorization(ctx, "sha256:authorization")
				return err
			},
		},
		{
			name: "image",
			read: func(tx *store.ReadTx) error {
				_, err := tx.GetProjectImage(ctx, "sha256:image")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readErr := st.Read(ctx, tc.read)
			if readErr == nil {
				t.Fatalf("%s malformed row reconstructed", tc.name)
			}
			classified := remediationSourceReadError(ctx, readErr)
			if !errors.Is(classified, errRemediationSourceIdentity) ||
				productionPublicationRetryableFailure(classified) {
				t.Fatalf("%s read = %v, want terminal source identity", tc.name, classified)
			}
		})
	}
}

func TestRemediationSourceReadPreservesContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := remediationSourceReadError(ctx, errors.New("store read failed"))
	if !errors.Is(err, context.Canceled) || errors.Is(err, errRemediationSourceIdentity) ||
		errors.Is(err, errProductionRetryable) {
		t.Fatalf("canceled source read = %v, want only context cancellation", err)
	}
}

func newQuarantineEngine(t *testing.T, ctx context.Context) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Engine{store: st, signet: signet.NewService(st)}, st
}

func productionOwnershipRun(runID domain.RunID) domain.Run {
	return domain.Run{
		ID: runID, ProjectID: "project-1",
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("a", 64)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		Stages: []domain.Stage{{
			ID: productionStageID(runID), RunID: runID,
			Name: productionStageName, Attempts: []domain.Attempt{},
		}},
	}
}

func seedProductionOwnershipRun(
	t *testing.T, ctx context.Context, st *store.Store, run domain.Run,
) {
	t.Helper()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("seed production ownership run: %v", err)
	}
}

func seedProductionMarker(
	t *testing.T, ctx context.Context, st *store.Store, runID domain.RunID, payload string,
) {
	t.Helper()
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			ctx, string(productionInvocationID(runID)),
			KindProductionInvocationRequested, []byte(payload),
		)
		return err
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
}

func requireQuarantineItem(
	t *testing.T, ctx context.Context, st *store.Store, runID domain.RunID,
) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, productionQuarantineItemID(runID))
		return err
	}); err != nil {
		t.Fatalf("read quarantine item for %q: %v", runID, err)
	}
	return item
}

func requireNoQuarantineItem(
	t *testing.T, ctx context.Context, st *store.Store, runID domain.RunID,
) {
	t.Helper()
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItemRecord(ctx, productionQuarantineItemID(runID))
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("quarantine item for %q = %v, want none", runID, err)
	}
}

func TestLoadProductionBindingAuthenticatesTheSpecificationInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(
		ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	specification := domain.Artifact{
		ID: "artifact-specification", Type: productionSpecificationArtifactType,
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: "submit-specification",
			HeadBinding:          domain.HeadIndependent,
			SensitivityClass:     domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}
	extra := specification
	extra.ID = "artifact-extra"
	extra.Type = domain.ArtifactKindEvidence
	extra.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	extra.Provenance.ProducerInvocationID = "foreign-producer"
	run := domain.Run{
		ID: "run-binding", ProjectID: "project-binding",
		SpecDigest:   specification.Digest,
		PolicyDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Stages: []domain.Stage{{
			ID: productionStageID("run-binding"), RunID: "run-binding",
			Name: productionStageName, Attempts: []domain.Attempt{},
		}},
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, extra); err != nil {
			return err
		}
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("seed binding state: %v", err)
	}

	tests := []struct {
		name    string
		inputs  []domain.ArtifactID
		wantErr bool
	}{
		{"canonical", []domain.ArtifactID{specification.ID}, false},
		{"extra input", []domain.ArtifactID{specification.ID, extra.ID}, true},
		{"foreign input", []domain.ArtifactID{extra.ID}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			invocationID := domain.InvocationID(
				"inv-implement-run-binding-" + strings.ReplaceAll(tc.name, " ", "-"),
			)
			invocation, err := domain.NewAgentInvocation(
				invocationID, tc.inputs, nil, 0,
			)
			if err != nil {
				t.Fatalf("new invocation: %v", err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAgentInvocation(ctx, invocation)
			}); err != nil {
				t.Fatalf("seed invocation: %v", err)
			}
			_, err = (&Engine{store: st}).loadProductionBinding(
				ctx, productionInvocationRequest{
					InvocationID: invocationID, RunID: run.ID,
					StageID: productionStageID(run.ID),
				},
			)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("binding error = %v, want ErrParentKeyMismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonical binding: %v", err)
			}
		})
	}
}

func TestProductionAcceptanceRequiresDurableAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := &Engine{store: st}
	if err := e.requireProductionAdmissible(ctx, "inv-implement-run-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing production admission error = %v, want store.ErrNotFound", err)
	}
}

// TestProductionQuarantineRecursAfterRelease: a concluded notice is history,
// not a record of the current hold. A second quarantine after a repair must
// raise its own open notice, or the run is held behind nothing an operator
// would read as current.
func TestProductionQuarantineRecursAfterRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-recurring")
	run := domain.Run{ID: runID, ProjectID: "project-1"}
	base := productionMarkerQuarantinePrefix

	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, run.ProjectID, productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("first quarantine: %v", err)
	}
	if err := releaseProductionQuarantine(ctx, st, e.signet, base, runID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if first := requireQuarantineItem(t, ctx, st, runID); first.Status != domain.StatusSuperseded {
		t.Fatalf("released notice = %#v", first)
	}

	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, run.ProjectID, productionQuarantineUnsupportedVersion,
	); err != nil {
		t.Fatalf("second quarantine: %v", err)
	}
	second, found, err := readProductionQuarantineItem(
		ctx, st, productionQuarantineOccurrenceID(base, runID, 2))
	if err != nil || !found {
		t.Fatalf("second occurrence = %v, %v", found, err)
	}
	if second.Status != domain.StatusOpen || second.Reason != productionQuarantineUnsupportedVersion {
		t.Fatalf("second occurrence = %#v", second)
	}

	// A repeated pass converges on that open occurrence instead of opening a third.
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, run.ProjectID, productionQuarantineUnsupportedVersion,
	); err != nil {
		t.Fatalf("replayed quarantine: %v", err)
	}
	if _, found, err := readProductionQuarantineItem(
		ctx, st, productionQuarantineOccurrenceID(base, runID, 3)); err != nil || found {
		t.Fatalf("replayed pass opened a third occurrence: %v, %v", found, err)
	}
}

// TestProductionQuarantineReleaseConvergesOnADecision: an operator concluding
// the notice while a pass releases it is a race the pass must absorb. Turning
// it into an error would end the reconcile loop this path exists to keep
// running.
func TestProductionQuarantineReleaseConvergesOnADecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-decided-race")
	base := productionMarkerQuarantinePrefix
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	stale := requireQuarantineItem(t, ctx, st, runID)

	// The operator's decision commits first.
	decided := stale
	decided.Status = domain.StatusResolved
	decided.ItemVersion = 2
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, decided)
	}); err != nil {
		t.Fatalf("record decision: %v", err)
	}

	// The release pass is now holding the stale open copy.
	if err := releaseProductionQuarantine(ctx, st, e.signet, base, runID); err != nil {
		t.Fatalf("release under a concurrent decision: %v", err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.Status != domain.StatusResolved || current.ItemVersion != 2 {
		t.Fatalf("decision was overwritten: %#v", current)
	}
}

// TestProductionQuarantineRejectsADivergentConcurrentItem: a lost create race
// is only converged when what is stored really is this run's notice.
func TestProductionQuarantineRejectsADivergentConcurrentItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-divergent")
	foreign, err := productionQuarantineItem(
		productionQuarantineItemID(runID), "run-other", "project-other", "Some other notice.")
	if err != nil {
		t.Fatalf("construct foreign item: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, foreign)
	}); err != nil {
		t.Fatalf("seed foreign item: %v", err)
	}

	err = confirmProductionQuarantineItem(
		ctx, st, productionQuarantineItemID(runID),
		domain.AttentionItem{
			ProjectID: "project-1", Type: domain.AttentionExecutionFailure,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID)},
		},
		store.ErrStaleWrite,
	)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("divergent concurrent item accepted: %v", err)
	}
}

// TestProductionQuarantineConvergesOnConcurrentCreationTime: each contender
// samples its own creation stamp, but the first durable notice owns that
// lifecycle fact and the losing pass must accept it without rewriting it.
func TestProductionQuarantineConvergesOnConcurrentCreationTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-concurrent-created-at")
	itemID := productionQuarantineItemID(runID)
	winner, err := productionQuarantineItem(
		itemID, runID, "project-1", productionQuarantineUnreadable)
	if err != nil {
		t.Fatalf("construct winner: %v", err)
	}
	winnerCreatedAt := time.Date(2026, 8, 14, 20, 0, 0, 0, time.UTC)
	winner.CreatedAt = &winnerCreatedAt
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, winner)
	}); err != nil {
		t.Fatalf("seed winner: %v", err)
	}

	loser := winner
	loserCreatedAt := winnerCreatedAt.Add(time.Second)
	loser.CreatedAt = &loserCreatedAt
	if err := confirmProductionQuarantineItem(
		ctx, st, itemID, loser, store.ErrStaleWrite,
	); err != nil {
		t.Fatalf("confirm concurrent winner: %v", err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.CreatedAt == nil || !current.CreatedAt.Equal(winnerCreatedAt) {
		t.Fatalf("winner created_at = %v, want %v", current.CreatedAt, winnerCreatedAt)
	}
}

// TestProductionQuarantineRefreshesTheOpenNotice: the open notice must
// describe the hold that is current. A marker that changes class before the
// notice is concluded would otherwise leave an operator reading that an
// upgrade repairs a marker which has since become malformed instead.
func TestProductionQuarantineRefreshesTheOpenNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-refreshed")
	base := productionMarkerQuarantinePrefix
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnsupportedVersion,
	); err != nil {
		t.Fatalf("first quarantine: %v", err)
	}
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("reclassified quarantine: %v", err)
	}
	current := requireQuarantineItem(t, ctx, st, runID)
	if current.Reason != productionQuarantineUnreadable || current.ItemVersion != 2 ||
		current.Status != domain.StatusOpen {
		t.Fatalf("refreshed notice = %#v", current)
	}

	// An unchanged condition writes nothing.
	if err := recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable,
	); err != nil {
		t.Fatalf("replayed quarantine: %v", err)
	}
	if replayed := requireQuarantineItem(t, ctx, st, runID); replayed.ItemVersion != 2 {
		t.Fatalf("replayed pass rewrote the notice: %#v", replayed)
	}
}

// TestProductionQuarantineRejectsADivergentOpenNotice: the replay path
// re-checks the stored row rather than trusting the identity it was found
// under.
func TestProductionQuarantineRejectsADivergentOpenNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-divergent-open")
	base := productionMarkerQuarantinePrefix
	foreign, err := productionQuarantineItem(
		productionQuarantineItemID(runID), "run-other", "project-1", "Some other notice.")
	if err != nil {
		t.Fatalf("construct foreign item: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, foreign)
	}); err != nil {
		t.Fatalf("seed foreign item: %v", err)
	}
	err = recordProductionQuarantine(
		ctx, st, e.signet, base, runID, "project-1", productionQuarantineUnreadable)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("divergent open notice accepted: %v", err)
	}
}

// TestProductionQuarantineOccurrenceIDsNeverCollide: a run id is validated
// only as non-empty, so run "foo" and run "foo-2" are an ordinary pair. An
// occurrence appended after the run id would give run "foo"'s second notice
// run "foo-2"'s identity, and the mismatched subject that produces is an
// error on the path whose whole purpose is to keep the loop running.
func TestProductionQuarantineOccurrenceIDsNeverCollide(t *testing.T) {
	t.Parallel()
	seen := map[domain.ItemID]string{}
	for _, runID := range []domain.RunID{"foo", "foo-2", "2-foo", "1", "12"} {
		for occurrence := 1; occurrence <= 13; occurrence++ {
			id := productionQuarantineOccurrenceID(productionMarkerQuarantinePrefix, runID, occurrence)
			key := fmt.Sprintf("%s#%d", runID, occurrence)
			if prior, dup := seen[id]; dup {
				t.Fatalf("id %q is shared by %s and %s", id, prior, key)
			}
			seen[id] = key
			if task := productionQuarantineOccurrenceID(
				productionTaskQuarantinePrefix, runID, occurrence); task == id {
				t.Fatalf("marker and task notices share id %q", id)
			}
		}
	}
}

// TestProductionQuarantineSurvivesADeepNoticeHistory: a run repaired and
// re-quarantined many times keeps getting a current open notice. A bounded
// history would have to choose between erroring, which ends the loop, and
// holding the run behind nothing an operator would read as current.
func TestProductionQuarantineSurvivesADeepNoticeHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-deep-history")
	for cycle := 1; cycle <= 40; cycle++ {
		if err := recordProductionQuarantine(
			ctx, st, e.signet, productionMarkerQuarantinePrefix,
			runID, "project-1", productionQuarantineUnreadable,
		); err != nil {
			t.Fatalf("quarantine cycle %d: %v", cycle, err)
		}
		current, found, err := readProductionQuarantineItem(
			ctx, st, productionQuarantineOccurrenceID(
				productionMarkerQuarantinePrefix, runID, cycle))
		if err != nil || !found || current.Status != domain.StatusOpen {
			t.Fatalf("cycle %d notice = %v, %v, %v", cycle, found, current.Status, err)
		}
		if err := releaseProductionQuarantine(
			ctx, st, e.signet, productionMarkerQuarantinePrefix, runID,
		); err != nil {
			t.Fatalf("release cycle %d: %v", cycle, err)
		}
	}
}

// TestProductionQuarantineReleaseLeavesForeignItemsAlone: the release path
// concludes only this hold's own notice. An unrelated item under the same
// predictable id is left untouched, and left alone rather than turned into an
// error, since failing to retire a notice this lane does not own is harmless
// while erroring would end the reconcile loop.
func TestProductionQuarantineReleaseLeavesForeignItemsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	for _, tc := range []struct {
		name   string
		runID  domain.RunID
		prefix string
		reason string
	}{
		{
			"unrelated reason", "run-foreign-reason", productionMarkerQuarantinePrefix,
			"An operator item that is not a quarantine.",
		},
		{
			"other row class", "run-foreign-class", productionMarkerQuarantinePrefix,
			productionQuarantineUnreadableTask,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := tc.runID
			id := productionQuarantineOccurrenceID(tc.prefix, runID, 1)
			foreign, err := productionQuarantineItem(id, runID, "project-1", tc.reason)
			if err != nil {
				t.Fatalf("construct item: %v", err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, foreign)
			}); err != nil {
				t.Fatalf("seed item: %v", err)
			}
			if err := releaseProductionQuarantine(ctx, st, e.signet, tc.prefix, runID); err != nil {
				t.Fatalf("release over a foreign item: %v", err)
			}
			current, found, err := readProductionQuarantineItem(ctx, st, id)
			if err != nil || !found {
				t.Fatalf("read back: %v, %v", found, err)
			}
			if current.Status != domain.StatusOpen || current.ItemVersion != 1 {
				t.Fatalf("foreign item was concluded: %#v", current)
			}
		})
	}
}

// TestProductionQuarantineRepairsADriftedNotice: the stored row is a
// reconstruction, so every operator-facing field is re-derived from the
// current hold. A row carrying this run's bindings and reason but a drifted
// priority, action set, or interruption class is repaired, not accepted: a
// subset check can only authenticate the fields someone thought to list.
func TestProductionQuarantineRepairsADriftedNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		runID domain.RunID
		drift func(*domain.AttentionItem)
	}{
		{"priority", "run-drift-priority", func(i *domain.AttentionItem) {
			i.Priority = domain.PriorityLow
		}},
		{"requested decision", "run-drift-actions", func(i *domain.AttentionItem) {
			i.RequestedDecision = []domain.Action{domain.ActionDiscuss}
		}},
		{"interruption class", "run-drift-class", func(i *domain.AttentionItem) {
			i.InterruptionClass = domain.InterruptionPlannedGate
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, st := newQuarantineEngine(t, ctx)
			drifted, err := productionQuarantineItem(
				productionQuarantineItemID(tc.runID), tc.runID, "project-1",
				productionQuarantineUnreadable)
			if err != nil {
				t.Fatalf("construct item: %v", err)
			}
			tc.drift(&drifted)
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, drifted)
			}); err != nil {
				t.Fatalf("seed drifted item: %v", err)
			}

			if err := recordProductionQuarantine(
				ctx, st, e.signet, productionMarkerQuarantinePrefix,
				tc.runID, "project-1", productionQuarantineUnreadable,
			); err != nil {
				t.Fatalf("record over a drifted notice: %v", err)
			}
			canonical, err := productionQuarantineItem(
				productionQuarantineItemID(tc.runID), tc.runID, "project-1",
				productionQuarantineUnreadable)
			if err != nil {
				t.Fatalf("construct canonical item: %v", err)
			}
			current := requireQuarantineItem(t, ctx, st, tc.runID)
			if !sameProductionQuarantineNotice(current, canonical) {
				t.Fatalf("drifted notice was accepted: %#v", current)
			}
			if current.Status != domain.StatusOpen || current.ItemVersion != 2 {
				t.Fatalf("repaired notice lifecycle = %#v", current)
			}
		})
	}
}

// TestProductionMarkerVersionClassifiedBeforeStrictDecode: a newer version
// normally adds a field, and the strict decode would reject that before the
// version was read, reporting the downgrade this lane exists to survive as a
// malformed marker. The classifier runs first, and only ever changes which
// refusal an operator reads.
func TestProductionMarkerVersionClassifiedBeforeStrictDecode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{
			name: "future version adding a field",
			payload: `{"version":"freeside.production-invocation/v3",` +
				`"invocation_id":"inv-implement-run-1","run_id":"run-1",` +
				`"stage_id":"implement-run-1","review":{"source":"codex"}}`,
			want: "freeside.production-invocation/v3",
		},
		{
			name: "future version renaming a field",
			payload: `{"version":"freeside.production-invocation/v4",` +
				`"invocation":"inv-implement-run-1","run_id":"run-1"}`,
			want: "freeside.production-invocation/v4",
		},
		{"released version", productionRequestJSON(
			`"version":"freeside.production-invocation/v2","invocation_id":"inv-implement-run-1",` +
				`"run_id":"run-1","stage_id":"implement-run-1"`), ""},
		{"unversioned preview", productionRequestJSON(
			`"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"`), ""},
		{"released legacy v1", `{"invocation_id":"inv-implement-run-1","run_id":"run-1","stage_id":"implement-run-1"}`, ""},
		{"obsolete namespace member", `{"version":"freeside.production-invocation/v1","run_id":"run-1"}`, ""},
		{"corrupt version", `{"version":"garbage","run_id":"run-1"}`, ""},
		{"foreign namespace", `{"version":"other.product/v9","run_id":"run-1"}`, ""},
		{"non-canonical number", `{"version":"freeside.production-invocation/v007","run_id":"run-1"}`, ""},
		{"suffixed version", `{"version":"freeside.production-invocation/v3-beta","run_id":"run-1"}`, ""},
		{"malformed json", `{"version":`, ""},
		{"non-string version", `{"version":9,"run_id":"run-1"}`, ""},
		{"empty version", `{"version":"","run_id":"run-1"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unsupportedProductionMarkerVersion([]byte(tc.payload)); got != tc.want {
				t.Fatalf("classified version = %q, want %q", got, tc.want)
			}
			_, err := decodeProductionRequest(productionEntry(tc.payload))
			if err == nil {
				return
			}
			if tc.want == "" && errors.Is(err, errProductionMarkerUnsupportedVersion) {
				t.Fatalf("released or malformed payload classified as a future version: %v", err)
			}
			if tc.want != "" && !errors.Is(err, errProductionMarkerUnsupportedVersion) {
				t.Fatalf("future version classified as unreadable: %v", err)
			}
		})
	}
}

// TestProductionMarkerVersionConstantsCompose pins the namespace, the release
// number, and the released version string to one another: the classifier
// decides the downgrade diagnosis from the first two, so a drift between them
// would silently change which markers this binary claims to implement.
func TestProductionMarkerVersionConstantsCompose(t *testing.T) {
	t.Parallel()
	composed := fmt.Sprintf("%s%d",
		productionInvocationVersionNamespace, productionInvocationRequestVersionNumber)
	if composed != productionInvocationRequestVersion {
		t.Fatalf("composed version = %q, want %q", composed, productionInvocationRequestVersion)
	}
}

// TestProductionQuarantineDiagnosesADowngrade ties the classification to what
// the operator actually reads: the notice for a newer marker names the
// upgrade that repairs the hold.
func TestProductionQuarantineDiagnosesADowngrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("run-downgraded")
	run := productionOwnershipRun(runID)
	seedProductionOwnershipRun(t, ctx, st, run)
	seedProductionMarker(t, ctx, st, runID, `{"version":"freeside.production-invocation/v3",`+
		`"invocation_id":"inv-implement-run-downgraded","run_id":"run-downgraded",`+
		`"stage_id":"implement-run-downgraded","review":{"source":"codex"}}`)

	owned, err := e.ownsProductionRun(ctx, run)
	if owned || err != nil {
		t.Fatalf("ownership = %v, %v", owned, err)
	}
	if item := requireQuarantineItem(t, ctx, st, runID); item.Reason != productionQuarantineUnsupportedVersion {
		t.Fatalf("quarantine reason = %q", item.Reason)
	}
}

// validPublicationTask is the minimum durable task decodeProductionPublicationTask
// accepts: it must survive full validation, because the acceptance scan reaches
// the dispatched-row check only after the task reconstructs.
func validPublicationTask(
	t *testing.T, runID domain.RunID, projectID domain.ProjectID,
) productionPublicationTask {
	t.Helper()
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	blob := export.Digest("sha256:" + strings.Repeat("c", 64))
	mode := "0644"
	size := int64(3)
	manifest := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{{
			Path: "README.md", Kind: export.EntryRegular,
			Mode: &mode, Size: &size, Digest: &blob,
		}},
	}
	encoded, err := manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return productionPublicationTask{
		Version: productionPublicationTaskVersion,
		RunID:   runID, ProjectID: projectID,
		ProducingInvocationID: productionInvocationID(runID),
		VerificationID:        productionVerificationInvocationID(runID),
		PublicationID:         productionPublicationInvocationID(runID),
		HeadSHA:               head,
		Replay: ProductionReplay{
			InvocationID:    productionInvocationID(runID),
			ObservedBaseSHA: base, HeadSHA: head,
			Manifest: manifest, ManifestDigest: digestProductionBytes(encoded),
			ImportOptions: importer.Options{
				BaseSHA: base, CommitDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		Publication: ProductionPublication{
			Title: "Publish the production run", Body: "Produced by a production run.\n",
			CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside", BotUserID: 42},
		},
	}
}

func TestReconstructProductionReevaluationTaskFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name            string
		action          domain.Action
		resolveItem     bool
		seedTask        bool
		dispatchTask    bool
		malformedSet    bool
		tamperArtifacts bool
		mutateRequest   func(*signet.PublicationReevaluationRequest)
		wantValid       bool
	}{
		{name: "valid", action: domain.ActionRerunTrustEvaluation, resolveItem: true, seedTask: true, dispatchTask: true, wantValid: true},
		{
			name: "forged payload run", action: domain.ActionRerunTrustEvaluation, resolveItem: true, seedTask: true, dispatchTask: true,
			mutateRequest: func(request *signet.PublicationReevaluationRequest) { request.RunID = "run-forged" },
		},
		{
			name: "forged trust profile", action: domain.ActionRerunTrustEvaluation, resolveItem: true, seedTask: true, dispatchTask: true,
			mutateRequest: func(request *signet.PublicationReevaluationRequest) { request.TrustProfileDigest = "sha256:forged" },
		},
		{name: "wrong action", action: domain.ActionStop, resolveItem: true, seedTask: true, dispatchTask: true},
		{name: "malformed action set", action: domain.ActionRerunTrustEvaluation, resolveItem: true, seedTask: true, dispatchTask: true, malformedSet: true},
		{name: "artifact binding mismatch", action: domain.ActionRerunTrustEvaluation, resolveItem: true, seedTask: true, dispatchTask: true, tamperArtifacts: true},
		{name: "open item", action: domain.ActionRerunTrustEvaluation, seedTask: true, dispatchTask: true},
		{name: "missing task", action: domain.ActionRerunTrustEvaluation, resolveItem: true},
		{name: "pending task", action: domain.ActionRerunTrustEvaluation, resolveItem: true, seedTask: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			st, err := store.Open(ctx, dbPath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			runID := domain.RunID("run-reevaluation-boundary")
			task := validPublicationTask(t, runID, "project-reevaluation-boundary")
			profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
				Repo: "acme/reevaluation", RepositoryID: 7,
				PRExecution:                domain.PRExecutionAuditedSameRepo,
				CandidateAutomationChanges: domain.AutomationChangesBlocked,
				PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
				CommitPlan:                 domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
				WorkflowAuditDigest: "sha256:workflow-audit",
				Review:              domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config"},
			})
			if err != nil {
				t.Fatal(err)
			}
			project, err := domain.NewProject(task.ProjectID, profile.Repo, profile.RepositoryID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if err := tx.RegisterProject(ctx, project); err != nil {
					return err
				}
				return tx.RecordTrustProfile(ctx, profile, time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC))
			}); err != nil {
				t.Fatal(err)
			}
			var claims []domain.AgentClaim
			if tc.tamperArtifacts {
				claims = []domain.AgentClaim{reevaluationBoundaryClaim("original evidence")}
			}
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: domain.ProductionBlockedItemID(runID), ProjectID: task.ProjectID,
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type:    domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
				Reason: domain.PublicationBlockVerification,
				RequestedDecision: []domain.Action{
					domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
				},
				AgentClaims: claims,
				PRHeadSHA:   task.HeadSHA, ItemVersion: 1,
				InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.malformedSet {
				item.RequestedDecision = []domain.Action{domain.ActionRerunTrustEvaluation}
			}
			command, err := domain.NewCommand(domain.CommandInput{
				CommandID: "cmd-reevaluate", DeviceID: "device-reevaluate", ItemID: item.ID,
				ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
				ArtifactDigests: item.ArtifactDigests, Action: tc.action,
			})
			if err != nil {
				t.Fatal(err)
			}
			resolvedItem := item
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
				if err := tx.PutCommand(ctx, command); err != nil {
					return err
				}
				if tc.resolveItem {
					resolvedItem.ItemVersion++
					resolvedItem.Status = domain.StatusResolved
					resolvedItem, err = resolvedItem.WithDecidedAt(time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC))
					if err != nil {
						return err
					}
					return tx.PutAttentionItem(ctx, resolvedItem)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if tc.tamperArtifacts {
				resolvedItem.AgentClaims = []domain.AgentClaim{reevaluationBoundaryClaim("tampered evidence")}
				resolvedItem.ArtifactDigests = []domain.Digest{resolvedItem.AgentClaims[0].Digest}
				body, err := json.Marshal(resolvedItem)
				if err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				_, updateErr := db.ExecContext(ctx,
					"UPDATE attention_items SET body = ? WHERE id = ?", string(body), item.ID,
				)
				closeErr := db.Close()
				if updateErr != nil || closeErr != nil {
					t.Fatal(errors.Join(updateErr, closeErr))
				}
			}
			if tc.seedTask {
				payload, err := json.Marshal(task)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
					if _, _, err := tx.EnqueueOutbox(ctx, productionPublicationTaskKey(runID), KindProductionPublicationRequested, payload); err != nil {
						return err
					}
					if tc.dispatchTask {
						return tx.MarkOutboxDispatched(ctx, productionPublicationTaskKey(runID))
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			request := signet.PublicationReevaluationRequest{
				RunID: runID, ItemID: item.ID, ItemVersion: item.ItemVersion,
				CommandID: command.CommandID, PRHeadSHA: item.PRHeadSHA,
				TrustProfileDigest: profile.ProfileDigest, ReviewRound: 1,
			}
			if tc.mutateRequest != nil {
				tc.mutateRequest(&request)
			}
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			entry := store.QueueEntry{
				IdempotencyKey: signet.PublicationReevaluationKey(runID, command.CommandID),
				Kind:           signet.PublicationReevaluationRequestedKind, Payload: payload,
			}
			before, err := st.ServerState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			workflow := &productionPublicationWorkflow{store: st}
			got, err := workflow.reconstructProductionReevaluationTask(ctx, entry)
			if tc.wantValid {
				if err != nil || got.reevaluation == nil || got.intentKey() != entry.IdempotencyKey {
					t.Fatalf("reconstruct = %#v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("reconstruct succeeded: %#v", got)
			}
			after, err := st.ServerState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("read boundary moved state from %#v to %#v", before, after)
			}
			if tc.name == "wrong action" {
				if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
					_, _, err := tx.EnqueueOutbox(
						ctx, entry.IdempotencyKey, entry.Kind, entry.Payload,
					)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				beforeReconcile, err := st.ServerState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := workflow.reconcile(ctx); err == nil {
					t.Fatal("reconcile accepted mismatched reevaluation intent")
				}
				afterReconcile, err := st.ServerState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if afterReconcile != beforeReconcile {
					t.Fatalf("reconcile mismatch moved state from %#v to %#v",
						beforeReconcile, afterReconcile)
				}
				workflow.holdOnly = true
				beforeHoldOnly, err := st.ServerState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := workflow.reconcile(ctx); err == nil {
					t.Fatal("hold-only reconcile accepted mismatched reevaluation intent")
				}
				afterHoldOnly, err := st.ServerState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if afterHoldOnly != beforeHoldOnly {
					t.Fatalf("hold-only mismatch moved state from %#v to %#v",
						beforeHoldOnly, afterHoldOnly)
				}
			}
		})
	}
}

func reevaluationBoundaryClaim(content string) domain.AgentClaim {
	text := domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: content}
	return domain.AgentClaim{
		Label: "reevaluation evidence", Artifact: domain.ArtifactID("artifact-" + content),
		Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-reevaluation-evidence",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: claimTextMeta(text),
	}
}

func TestDecodeProductionPublicationTaskPreservesLegacyRemediationNoop(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-legacy-remediation-noop")
	task := validPublicationTask(t, runID, "project-legacy-remediation-noop")
	task.ProducingInvocationID = remediationInvocationID(runID, 1)
	task.LegacyRemediationNoop = true
	task.VerificationID = productionVerificationInvocationIDForProducer(
		runID, task.ProducingInvocationID,
	)
	task.Replay.InvocationID = task.ProducingInvocationID
	payload, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeProductionPublicationTask(store.QueueEntry{
		IdempotencyKey: productionPublicationTaskKey(runID),
		Kind:           KindProductionPublicationRequested,
		Payload:        payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.LegacyRemediationNoop {
		t.Fatal("legacy remediation_noop field was not preserved")
	}
}

func TestProductionPublicationCompletionAuthenticatesTaskAndTerminal(t *testing.T) {
	t.Parallel()
	run := domain.Run{ID: "run-supervision-completion", ProjectID: "project-supervision"}
	task := validPublicationTask(t, run.ID, run.ProjectID)
	taskPayload, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	terminal := productionTerminalRecord{
		InvocationID: task.ProducingInvocationID, RunID: task.RunID,
		StageID: productionStageID(task.RunID), Status: exec.StatusCompleted,
		HeadSHA: task.HeadSHA, Artifacts: task.Artifacts, Summary: task.Summary,
	}
	terminalPayload, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name            string
		taskPayload     []byte
		terminalPayload []byte
		dispatch        bool
		wantComplete    bool
		wantErr         error
	}{
		{name: "pending", taskPayload: taskPayload},
		{
			name: "authenticated completion", taskPayload: taskPayload,
			terminalPayload: terminalPayload, dispatch: true, wantComplete: true,
		},
		{
			name: "malformed task", taskPayload: []byte(`{}`),
			dispatch: true, wantErr: domain.ErrParentKeyMismatch,
		},
		{
			name: "forged publication invocation",
			taskPayload: func() []byte {
				changed := task
				changed.PublicationID = "publish-production-forged"
				payload, marshalErr := json.Marshal(changed)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				return payload
			}(),
			wantErr: domain.ErrParentKeyMismatch,
		},
		{
			name: "missing terminal", taskPayload: taskPayload,
			dispatch: true, wantErr: domain.ErrImmutableTransition,
		},
		{
			name: "divergent terminal", taskPayload: taskPayload,
			terminalPayload: func() []byte {
				changed := terminal
				changed.Summary = "foreign summary"
				payload, marshalErr := json.Marshal(changed)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				return payload
			}(),
			dispatch: true, wantErr: domain.ErrParentKeyMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if _, _, err := tx.EnqueueOutbox(
					ctx, productionPublicationTaskKey(run.ID),
					KindProductionPublicationRequested, tc.taskPayload,
				); err != nil {
					return err
				}
				if tc.terminalPayload != nil {
					if _, _, err := tx.RecordInbox(
						ctx, string(task.ProducingInvocationID),
						kindProductionStageTerminal, tc.terminalPayload,
					); err != nil {
						return err
					}
				}
				if tc.dispatch {
					return tx.MarkOutboxDispatched(ctx, productionPublicationTaskKey(run.ID))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				identity, complete, err := ProductionPublicationCompletion(ctx, tx, run)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("completion error = %v, want %v", err, tc.wantErr)
				}
				wantIdentity := ProductionPublicationIdentity{}
				if tc.wantComplete {
					wantIdentity = ProductionPublicationIdentity{
						ProducingInvocationID:   task.ProducingInvocationID,
						PublicationInvocationID: task.PublicationID,
					}
				}
				if err == nil && (complete != tc.wantComplete || identity != wantIdentity) {
					t.Fatalf("completion = %+v, %v, want %+v, %v",
						identity, complete, wantIdentity, tc.wantComplete)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewAttentionReusesFirstClassifierRoutingDecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		first       domain.AttentionType
		second      domain.AttentionType
		firstReason string
	}{
		{
			name:  "fallback then recovered classification",
			first: domain.AttentionReviewDispute, second: domain.AttentionReviewDiminishing,
			firstReason: "classification was unavailable",
		},
		{
			name:  "classification then conservative retry",
			first: domain.AttentionReviewDiminishing, second: domain.AttentionReviewDispute,
			firstReason: "classification did not require attention",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			w := &productionPublicationWorkflow{store: st, attention: signet.NewService(st)}
			task := productionPublicationTask{
				RunID: "run-classifier-retry", ProjectID: "project-classifier-retry",
				HeadSHA: strings.Repeat("a", 40),
			}
			record := domain.ReviewRecord{Round: 1}
			if err := w.putReviewAttention(ctx, task, record, tc.firstReason, tc.first); err != nil {
				t.Fatalf("put first routing decision: %v", err)
			}
			if err := w.putReviewAttention(ctx, task, record, "retry chose another route", tc.second); err != nil {
				t.Fatalf("reuse first routing decision: %v", err)
			}
			var item domain.AttentionItem
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var readErr error
				item, readErr = tx.GetAttentionItem(ctx, productionReviewItemID(task.RunID, record.Round))
				return readErr
			}); err != nil {
				t.Fatal(err)
			}
			if item.Type != tc.first || item.Reason != tc.firstReason || item.ItemVersion != 1 {
				t.Fatalf("reused attention = %#v", item)
			}
		})
	}
	t.Run("rejects a changed candidate binding", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		w := &productionPublicationWorkflow{store: st, attention: signet.NewService(st)}
		task := productionPublicationTask{
			RunID: "run-classifier-retry", ProjectID: "project-classifier-retry",
			HeadSHA: strings.Repeat("a", 40),
		}
		record := domain.ReviewRecord{Round: 1}
		if err := w.putReviewAttention(
			ctx, task, record, "first decision", domain.AttentionReviewDispute,
		); err != nil {
			t.Fatal(err)
		}
		task.HeadSHA = strings.Repeat("b", 40)
		if err := w.putReviewAttention(
			ctx, task, record, "changed candidate", domain.AttentionReviewDispute,
		); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("changed candidate binding error = %v", err)
		}
	})
	for _, status := range []domain.ItemStatus{domain.StatusOpen, domain.StatusResolved} {
		t.Run("legacy dispute "+string(status), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			attention := signet.NewService(st)
			w := &productionPublicationWorkflow{store: st, attention: attention}
			task := productionPublicationTask{
				RunID: "run-classifier-retry", ProjectID: "project-classifier-retry",
				HeadSHA: strings.Repeat("a", 40),
			}
			record := domain.ReviewRecord{Round: 1}
			runID := task.RunID
			legacy, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: productionReviewItemID(task.RunID, record.Round), ProjectID: task.ProjectID,
				Subject: domain.Subject{
					Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
				},
				Type: domain.AttentionReviewDispute, Priority: domain.PriorityNormal,
				Reason: "legacy dispute",
				RequestedDecision: []domain.Action{
					domain.ActionDiscuss, domain.ActionStop,
				},
				PRHeadSHA: task.HeadSHA, ItemVersion: 1,
				InterruptionClass: domain.InterruptionPlannedGate, Status: status,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := attention.PutItem(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			if err := w.putReviewAttention(
				ctx, task, record, "retry chose diminishing", domain.AttentionReviewDiminishing,
			); err != nil {
				t.Fatalf("reuse legacy dispute: %v", err)
			}
			var item domain.AttentionItem
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var readErr error
				item, readErr = tx.GetAttentionItem(ctx, legacy.ID)
				return readErr
			}); err != nil {
				t.Fatal(err)
			}
			if item.Type != domain.AttentionReviewDispute || item.Reason != legacy.Reason ||
				item.Status != status {
				t.Fatalf("reused legacy dispute = %#v", item)
			}
			if item.ItemVersion != 1 || !item.Offers(domain.ActionDiscuss) ||
				!item.Offers(domain.ActionStop) {
				t.Fatalf("routed dispute changed = %#v", item)
			}
		})
	}
}

// TestQueuedCompletionToleratesAConcurrentPublicationDispatch: the publication
// lane commits its terminal and dispatches its task in two transactions, and
// since issue #425 it does so on its own loop. The acceptance scan can
// therefore read the inbox before the terminal commits and the outbox after
// the dispatch. That interleaving is a publication finishing beside the scan,
// not the "dispatched without a terminal" violation; reading it as the
// violation would stop Engine.Run at the successful end of a publication. The
// real violation, a dispatched task with no terminal at all, must stay loud.
func TestQueuedCompletionToleratesAConcurrentPublicationDispatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	run := domain.Run{ID: "run-concurrent-dispatch", ProjectID: "project-concurrent-dispatch"}
	for _, tc := range []struct {
		name     string
		terminal bool
	}{
		{"terminal committed before the dispatch was observed", true},
		{"no terminal at all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			w := &productionPublicationWorkflow{store: st, attention: signet.NewService(st)}

			task := validPublicationTask(t, run.ID, run.ProjectID)
			payload, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			key := productionPublicationTaskKey(run.ID)
			if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if _, _, err := tx.EnqueueOutbox(
					ctx, key, KindProductionPublicationRequested, payload,
				); err != nil {
					return err
				}
				if tc.terminal {
					if _, _, err := tx.RecordInbox(
						ctx, string(task.ProducingInvocationID),
						kindProductionStageTerminal, []byte(`{}`),
					); err != nil {
						return err
					}
				}
				return tx.MarkOutboxDispatched(ctx, key)
			}); err != nil {
				t.Fatal(err)
			}

			queued, err := w.hasQueuedCompletion(ctx, run, task.ProducingInvocationID)
			if tc.terminal {
				if err != nil || !queued {
					t.Fatalf("concurrent dispatch = %v, %v; want owned with no error", queued, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrImmutableTransition) {
				t.Fatalf("dispatch without a terminal = %v, %v; want ErrImmutableTransition", queued, err)
			}
		})
	}
}

// TestProductionStageNameResolvesToCanonicalRole ties the §5.4 stage-role
// resolver's exhaustive legacy set to the engine's persisted spelling: the
// lineup keys resolve per role through CanonicalStageRole, and this is the
// one legacy name the engine writes.
func TestProductionStageNameResolvesToCanonicalRole(t *testing.T) {
	role, err := domain.CanonicalStageRole(productionStageName)
	if err != nil || role != domain.StageNameImplementation {
		t.Fatalf("CanonicalStageRole(%q) = %q, %v; want %q",
			productionStageName, role, err, domain.StageNameImplementation)
	}
}
