package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type immediateShadowSource struct {
	result         exec.ReviewResult
	requestError   error
	inspectError   error
	authorityError error
	teardownErrors []error
	status         exec.Status
	pollHook       func()
	requested      bool
	teardowns      int
	request        exec.ReviewRequest
	id             domain.InvocationID
}

func (s *immediateShadowSource) RequestReview(
	_ context.Context, id domain.InvocationID, request exec.ReviewRequest,
) error {
	if s.requestError != nil {
		return s.requestError
	}
	s.requested, s.request, s.id = true, request, id
	return nil
}

func (s *immediateShadowSource) Inspect(
	context.Context, domain.InvocationID,
) (exec.Status, error) {
	if !s.requested {
		return "", exec.ErrUnknownInvocation
	}
	if s.inspectError != nil {
		return "", s.inspectError
	}
	if s.status != "" {
		return s.status, nil
	}
	return exec.StatusCompleted, nil
}

func (s *immediateShadowSource) Poll(
	context.Context, domain.InvocationID,
) (exec.ReviewResult, error) {
	if s.pollHook != nil {
		s.pollHook()
	}
	result := s.result
	result.InvocationID = s.id
	result.BaseSHA = s.request.BaseSHA
	result.HeadSHA = s.request.HeadSHA
	result.InstructionDigest = s.request.Instructions.ResultDigest
	return result, nil
}

func (s *immediateShadowSource) Verify(
	_ context.Context, id domain.InvocationID, base, head string,
) error {
	if id != s.id || base != s.request.BaseSHA || head != s.request.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (s *immediateShadowSource) VerifyRequestAuthority(
	_ context.Context, _ domain.InvocationID, expected domain.Digest,
) error {
	if !s.requested {
		return exec.ErrUnknownInvocation
	}
	actual, err := s.request.AuthorityDigest()
	if err != nil {
		return err
	}
	if actual == expected && s.authorityError != nil {
		return s.authorityError
	}
	if actual != expected {
		if len(s.teardownErrors) > 0 {
			err := s.teardownErrors[0]
			s.teardownErrors = s.teardownErrors[1:]
			return err
		}
		s.teardowns++
		s.requested = false
		return domain.ErrParentKeyMismatch
	}
	return nil
}

type retainingShadowTransport struct{}

func (retainingShadowTransport) FetchBase(
	context.Context, string, string, string, string,
) (PublicationCheckout, error) {
	return nil, errors.New("unexpected fetch")
}

func (retainingShadowTransport) RetainWorktree(
	_ context.Context, _ PublicationCheckout, dest, _ string,
) error {
	return os.MkdirAll(dest, 0o700)
}

func (retainingShadowTransport) PushHead(
	context.Context, PublicationCheckout, publish.GatedHead,
) (publish.PushResult, error) {
	return publish.PushResult{}, errors.New("unexpected push")
}

func TestShadowReviewRecordsClassifiesSamplesAndBlocksReady(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st := storetest.Open(t, filepath.Join(dir, "freeside.db"), store.Options{})
	now := time.Date(2026, 8, 24, 14, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-shadow-review")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: shadowReviewRatePolicyKey, Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: shadowTestDigest("1"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-shadow",
		SpecDigest: shadowTestDigest("2"), PolicyDigest: policy.Digest,
	}
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	routedFinding := domain.Finding{
		ID: "routed-finding", RunID: runID, Source: "codex_local",
		Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 7, EndLine: 7},
		Message:  "shared material defect", RawText: "shared material defect", CreatedAt: now,
	}
	unrelatedRoutedFinding := routedFinding
	unrelatedRoutedFinding.ID = "unrelated-routed-finding"
	unrelatedRoutedFinding.Message = "unrelated routed defect"
	unrelatedRoutedFinding.RawText = "unrelated routed defect"
	routed, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(runID, 1), RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: shadowTestDigest("3"), InstructionDigest: shadowTestDigest("4"),
		CostOwner: "routed", BaseSHA: baseSHA, HeadSHA: headSHA, CompletedAt: now,
		CompletionEvidence: shadowTestDigest("5"), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{routedFinding.ID, unrelatedRoutedFinding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, domain.Device{
			ID: "device-shadow-review", DisplayName: "Operator",
			Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, []domain.Finding{
			routedFinding, unrelatedRoutedFinding,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	shadowFinding := routedFinding
	shadowFinding.ID = "shadow-finding"
	shadowFinding.Source = string(domain.ShadowReviewClaudeLocal)
	shadowFinding.CreatedAt = now.Add(time.Minute)
	lowerShadowFinding := shadowFinding
	lowerShadowFinding.ID = "shadow-finding-lower"
	lowerShadowFinding.Severity = domain.FindingSeverityP2
	lowerShadowFinding.Message = "lower-severity observation"
	lowerShadowFinding.RawText = "lower-severity observation"
	shadowDigest := shadowTestDigest("6")
	source := &immediateShadowSource{result: exec.ReviewResult{
		Provider: "anthropic", ModelConfiguration: "claude/test",
		ConfigurationDigest: shadowDigest, CostOwner: "shadow-budget",
		CompletedAt: now.Add(time.Minute), CompletionEvidence: shadowTestDigest("7"),
		Findings: []domain.Finding{shadowFinding, lowerShadowFinding},
	}}
	var failures int
	attention := signet.NewService(st, signet.WithClock(func() time.Time {
		return now.Add(3 * time.Minute)
	}))
	w := &productionPublicationWorkflow{
		store: st, attention: attention, workDir: dir,
		transport: retainingShadowTransport{}, now: func() time.Time { return now.Add(2 * time.Minute) },
		shadowReviewSource: source, shadowReviewConfigurationDigest: shadowDigest,
		shadowReviewCostOwner: "shadow-budget", shadowReviewDefaultRate: 0.2,
		shadowReviewFailure: func(domain.RunID, int, domain.ReviewFailureClass, error) { failures++ },
	}
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := productionBinding{
		run: run, resolvedPolicy: policy,
		admission: domain.ExecutionAdmission{Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: baseSHA,
		}},
	}
	checkpoint := productionVerificationCheckpoint{Authorization: domain.CandidateAuthorization{
		VerificationRecipeDigest: shadowTestDigest("8"),
		EvidenceSnapshotDigest:   shadowTestDigest("9"),
		VerificationOutcome:      domain.VerificationPassed,
	}}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, HeadSHA: headSHA}
	complete, err := w.reconcileShadowReview(
		ctx, task, binding, checkpoint, nil, instructions, routed,
	)
	if err != nil || !complete {
		t.Fatalf("reconcileShadowReview = %v, %v", complete, err)
	}
	if failures != 0 {
		t.Fatalf("shadow failures = %d", failures)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		records, err := tx.ListShadowReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		if len(records) != 1 || records[0].ShadowedRound != 1 || records[0].Outcome != domain.ReviewFindings {
			t.Fatalf("shadow records = %#v", records)
		}
		samples, err := tx.ListClassifierAccuracySamples(ctx, runID)
		if err != nil {
			return err
		}
		if len(samples) != 2 || samples[0].Assessment != domain.ClassifierAssessmentAccurate ||
			samples[1].Assessment != domain.ClassifierAssessmentIndeterminate {
			t.Fatalf("classifier samples = %#v", samples)
		}
		if _, err := tx.GetClassification(ctx, routedFinding.ID, routed.Round); err != nil {
			t.Fatalf("matching routed classification: %v", err)
		}
		if _, err := tx.GetClassification(
			ctx, unrelatedRoutedFinding.ID, routed.Round,
		); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unrelated routed classification = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	latest, _, err := w.latestReviewState(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.InvocationID != routed.InvocationID || latest.Round != 1 {
		t.Fatalf("routed review state advanced by shadow: %#v", latest)
	}
	if err := w.shadowReviewBlocksReady(ctx, task, binding); !errors.Is(err, errShadowReviewBlocksReady) {
		t.Fatalf("open shadow attention ready block = %v", err)
	}
	itemID := shadowReviewAttentionItemID(ProductionShadowReviewInvocationID(runID, 1))
	var item domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(ctx, itemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantActions := []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop}
	if !slices.Equal(item.RequestedDecision, wantActions) || item.Offers(domain.Action("adjudicate")) {
		t.Fatalf("shadow dispute actions = %v, want %v without adjudicate",
			item.RequestedDecision, wantActions)
	}
	if len(item.AgentClaims) != 1 || item.AgentClaims[0].Text == nil ||
		!strings.Contains(item.AgentClaims[0].Text.Content, "shared material defect") ||
		!strings.Contains(item.AgentClaims[0].Text.Content, "daemon/a.go:7") ||
		len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != item.AgentClaims[0].Digest {
		t.Fatalf("shadow dispute evidence = claims %#v, digests %v",
			item.AgentClaims, item.ArtifactDigests)
	}
	if !shadowReviewAttentionPresentsClaims(item, item.AgentClaims) {
		t.Fatal("exact shadow dispute claims were rejected")
	}
	var shadowRecords []domain.ShadowReviewRecord
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		shadowRecords, err = tx.ListShadowReviewRecords(ctx, runID)
		return err
	}); err != nil || len(shadowRecords) != 1 {
		t.Fatalf("shadow records for repair = %#v, %v", shadowRecords, err)
	}
	legacy := item
	legacy.AgentClaims = []domain.AgentClaim{}
	legacy.ArtifactDigests = []domain.Digest{}
	legacy.ItemVersion++
	if err := attention.PutItem(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := w.finishShadowReviewEvidence(ctx, task, routed, shadowRecords[0]); err != nil {
		t.Fatal(err)
	}
	repairedSnapshot, err := attention.GetAttentionItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	repaired := repairedSnapshot.Item
	if repaired.ItemVersion != legacy.ItemVersion+1 ||
		!shadowReviewAttentionPresentsClaims(repaired, item.AgentClaims) {
		t.Fatalf("repaired shadow dispute = version %d, claims %#v",
			repaired.ItemVersion, repaired.AgentClaims)
	}
	if err := w.finishShadowReviewEvidence(ctx, task, routed, shadowRecords[0]); err != nil {
		t.Fatal(err)
	}
	stableSnapshot, err := attention.GetAttentionItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	item = stableSnapshot.Item
	if item.ItemVersion != repaired.ItemVersion {
		t.Fatalf("idempotent shadow dispute version = %d, want %d",
			item.ItemVersion, repaired.ItemVersion)
	}
	snapshot, err := attention.GetAttentionItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attention.Submit(ctx, signet.ClientCommand{
		CommandID: "command-stop-shadow-review", DeviceID: "device-shadow-review",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Action: domain.ActionStop,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.shadowReviewBlocksReady(ctx, task, binding); !errors.Is(err, errShadowReviewBlocksReady) || !errors.Is(err, errShadowReviewStopped) {
		t.Fatalf("stopped shadow attention ready block = %v", err)
	}
}

func TestSplitShadowClaimTextPreservesBoundedUTF8(t *testing.T) {
	content := strings.Repeat("é", domain.MaxClaimTextBytes/2+1)
	chunks := splitShadowClaimText(content, domain.MaxClaimTextBytes)
	if len(chunks) != 2 || strings.Join(chunks, "") != content {
		t.Fatalf("shadow claim chunks = %d, joined equality %v", len(chunks), strings.Join(chunks, "") == content)
	}
	for idx, chunk := range chunks {
		if !utf8.ValidString(chunk) || len(chunk) > domain.MaxClaimTextBytes {
			t.Fatalf("shadow claim chunk %d = %d bytes, valid UTF-8 %v",
				idx, len(chunk), utf8.ValidString(chunk))
		}
	}
	record := domain.ShadowReviewRecord{
		InvocationID: "invocation-shadow-claim", HeadSHA: strings.Repeat("b", 40),
	}
	claims, err := shadowReviewFindingClaims(record, domain.Finding{
		ID: "finding-shadow-claim", Severity: domain.FindingSeverityP1,
		Message: "concise summary", RawText: "complete reviewer explanation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Text == nil ||
		!strings.Contains(claims[0].Text.Content, "concise summary") ||
		!strings.Contains(claims[0].Text.Content, "complete reviewer explanation") {
		t.Fatalf("distinct shadow finding text = %#v", claims)
	}
}

func TestShadowReviewFindingNeedsAttentionUsesClassifierCeiling(t *testing.T) {
	classification := domain.Classification{
		FindingID: "finding-shadow-attention", Version: 1,
		Materiality: string(inference.OrdinalHigh), Confidence: string(inference.OrdinalLow),
		Note: "producer=deterministic/fallback; conservative classification",
	}
	for _, test := range []struct {
		name     string
		severity domain.FindingSeverity
		want     bool
	}{
		{name: "critical high ceiling", severity: domain.FindingSeverityP1, want: true},
		{name: "lower severity only", severity: domain.FindingSeverityP2, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			finding := domain.Finding{
				ID: classification.FindingID, Source: string(domain.ShadowReviewClaudeLocal),
				Severity: test.severity,
			}
			got, err := shadowReviewFindingNeedsAttention(finding, classification)
			if err != nil || got != test.want {
				t.Fatalf("shadow attention predicate = %v, %v, want %v", got, err, test.want)
			}
		})
	}
}

func TestPersistedLowerShadowReviewRecoversBeforeDisabledLaunchPolicy(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st := storetest.Open(t, filepath.Join(dir, "freeside.db"), store.Options{})
	now := time.Date(2026, 8, 24, 16, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-shadow-recovery-disabled")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: shadowReviewRatePolicyKey, Value: "0",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: shadowTestDigest("1"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-shadow-recovery",
		SpecDigest: shadowTestDigest("2"), PolicyDigest: policy.Digest,
	}
	baseSHA, headSHA := strings.Repeat("c", 40), strings.Repeat("d", 40)
	routed, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(runID, 1), RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: shadowTestDigest("3"), InstructionDigest: shadowTestDigest("4"),
		CostOwner: "routed", BaseSHA: baseSHA, HeadSHA: headSHA, CompletedAt: now,
		CompletionEvidence: shadowTestDigest("5"), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	shadowDigest := shadowTestDigest("6")
	shadowFinding := domain.Finding{
		ID: "shadow-finding-lower-recovery", RunID: runID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "daemon/lower.go", StartLine: 9, EndLine: 9},
		Message:  "lower observation", RawText: "lower observation", CreatedAt: now.Add(time.Minute),
	}
	shadowRecord, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: ProductionShadowReviewInvocationID(runID, 1), RunID: runID, ShadowedRound: 1,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration: "claude/test", ConfigurationDigest: shadowDigest,
		InstructionDigest: instructions.ResultDigest, CostOwner: "shadow-budget",
		BaseSHA: baseSHA, HeadSHA: headSHA, CompletedAt: now.Add(time.Minute),
		CompletionEvidence: shadowTestDigest("7"), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{shadowFinding.ID},
	})
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
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadowRecord, []domain.Finding{shadowFinding})
	}); err != nil {
		t.Fatal(err)
	}
	w := &productionPublicationWorkflow{
		store: st, attention: signet.NewService(st), workDir: dir,
		now: func() time.Time { return now.Add(2 * time.Minute) },
	}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, HeadSHA: headSHA}
	binding := productionBinding{
		run: run, resolvedPolicy: policy,
		admission: domain.ExecutionAdmission{Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: baseSHA,
		}},
	}
	complete, err := w.reconcileShadowReview(
		ctx, task, binding, productionVerificationCheckpoint{}, nil, instructions, routed,
	)
	if err != nil || !complete {
		t.Fatalf("recover persisted shadow review = %v, %v", complete, err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetClassification(ctx, shadowFinding.ID, 1); err != nil {
			return err
		}
		samples, err := tx.ListClassifierAccuracySamples(ctx, runID)
		if err != nil {
			return err
		}
		if len(samples) != 1 || samples[0].FindingID != shadowFinding.ID {
			t.Fatalf("recovered classifier samples = %#v", samples)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.shadowReviewBlocksReady(ctx, task, binding); err != nil {
		t.Fatalf("lower-only recovered shadow review blocked ready: %v", err)
	}
}

func TestShadowReviewFailureIsClassifiedAndDoesNotBlockRoutedGate(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-shadow-failure")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: shadowReviewRatePolicyKey, Value: "1",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: shadowTestDigest("a")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: runID, ProjectID: "project-shadow", SpecDigest: shadowTestDigest("b"), PolicyDigest: policy.Digest}
	routed, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(runID, 1), RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: shadowTestDigest("c"), InstructionDigest: shadowTestDigest("d"),
		CostOwner: "routed", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		CompletedAt: time.Unix(1, 0).UTC(), CompletionEvidence: shadowTestDigest("e"), Outcome: domain.ReviewClean,
	})
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
		return tx.PutReviewRecord(ctx, routed, nil)
	}); err != nil {
		t.Fatal(err)
	}
	declared := &exec.ReviewSourceFailure{Class: domain.ReviewFailureQuota, Err: errors.New("quota exhausted")}
	source := &immediateShadowSource{status: exec.StatusRunning}
	var gotClass domain.ReviewFailureClass
	w := &productionPublicationWorkflow{
		store: st, attention: signet.NewService(st), workDir: t.TempDir(),
		transport: retainingShadowTransport{}, shadowReviewSource: source,
		shadowReviewConfigurationDigest: shadowTestDigest("f"),
		shadowReviewCostOwner:           "shadow", shadowReviewDefaultRate: 1,
		now: func() time.Time { return time.Unix(2, 0).UTC() },
		shadowReviewFailure: func(_ domain.RunID, _ int, class domain.ReviewFailureClass, _ error) {
			gotClass = class
		},
	}
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := productionBinding{run: run, resolvedPolicy: policy, admission: domain.ExecutionAdmission{Base: domain.BaseRevision{
		Repo: "owner/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: routed.BaseSHA,
	}}}
	checkpoint := productionVerificationCheckpoint{Authorization: domain.CandidateAuthorization{
		VerificationRecipeDigest: shadowTestDigest("0"), EvidenceSnapshotDigest: shadowTestDigest("1"),
		VerificationOutcome: domain.VerificationPassed,
	}}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, HeadSHA: routed.HeadSHA}
	complete, err := w.reconcileShadowReview(ctx,
		task,
		binding, checkpoint, nil, instructions, routed)
	if err != nil || complete {
		t.Fatalf("started shadow routing = complete %v, err %v", complete, err)
	}
	teardownPending := errors.New("shadow teardown still converging")
	source.authorityError = declared
	source.teardownErrors = []error{teardownPending}
	complete, err = w.reconcileShadowReview(ctx, task, binding, checkpoint, nil, instructions, routed)
	if !errors.Is(err, teardownPending) || complete || gotClass != "" || !source.requested {
		t.Fatalf("pending shadow teardown = complete %v, class %q, requested %v, err %v",
			complete, gotClass, source.requested, err)
	}
	complete, err = w.reconcileShadowReview(ctx, task, binding, checkpoint, nil, instructions, routed)
	if err != nil || !complete || gotClass != domain.ReviewFailureQuota {
		t.Fatalf("failed shadow routing = complete %v, class %q, err %v", complete, gotClass, err)
	}
	if source.teardowns != 1 || source.requested {
		t.Fatalf("started shadow teardown = %d, requested %v", source.teardowns, source.requested)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		records, err := tx.ListShadowReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			t.Fatalf("failed shadow records = %#v", records)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	latest, _, err := w.latestReviewState(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.InvocationID != routed.InvocationID {
		t.Fatalf("routed review changed by shadow failure: %#v", latest)
	}
}

func TestLateCompletedShadowReviewExpiresWithoutBlockingRoutedGate(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-shadow-timeout")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: shadowReviewRatePolicyKey, Value: "1",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: shadowTestDigest("2")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: runID, ProjectID: "project-shadow", SpecDigest: shadowTestDigest("3"), PolicyDigest: policy.Digest}
	completedAt := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	routed, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(runID, 1), RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: shadowTestDigest("4"), InstructionDigest: shadowTestDigest("5"),
		CostOwner: "routed", BaseSHA: strings.Repeat("3", 40), HeadSHA: strings.Repeat("4", 40),
		CompletedAt: completedAt, CompletionEvidence: shadowTestDigest("6"), Outcome: domain.ReviewClean,
	})
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
		return tx.PutReviewRecord(ctx, routed, nil)
	}); err != nil {
		t.Fatal(err)
	}
	current := completedAt.Add(time.Minute)
	shadowDigest := shadowTestDigest("7")
	source := &immediateShadowSource{status: exec.StatusRunning, result: exec.ReviewResult{
		Provider: "anthropic", ModelConfiguration: "claude/test",
		ConfigurationDigest: shadowDigest, CostOwner: "shadow",
		CompletedAt: completedAt.Add(shadowReviewWait), CompletionEvidence: shadowTestDigest("0"),
	}}
	var gotClass domain.ReviewFailureClass
	w := &productionPublicationWorkflow{
		store: st, attention: signet.NewService(st), workDir: t.TempDir(),
		transport: retainingShadowTransport{}, shadowReviewSource: source,
		shadowReviewConfigurationDigest: shadowDigest,
		shadowReviewCostOwner:           "shadow", shadowReviewDefaultRate: 1,
		now: func() time.Time { return current },
		shadowReviewFailure: func(_ domain.RunID, _ int, class domain.ReviewFailureClass, _ error) {
			gotClass = class
		},
	}
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := productionBinding{run: run, resolvedPolicy: policy, admission: domain.ExecutionAdmission{Base: domain.BaseRevision{
		Repo: "owner/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: routed.BaseSHA,
	}}}
	checkpoint := productionVerificationCheckpoint{Authorization: domain.CandidateAuthorization{
		VerificationRecipeDigest: shadowTestDigest("8"), EvidenceSnapshotDigest: shadowTestDigest("9"),
		VerificationOutcome: domain.VerificationPassed,
	}}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, HeadSHA: routed.HeadSHA}
	complete, err := w.reconcileShadowReview(ctx, task, binding, checkpoint, nil, instructions, routed)
	if err != nil || complete {
		t.Fatalf("pending shadow review = complete %v, err %v", complete, err)
	}
	source.status = exec.StatusCompleted
	current = completedAt.Add(shadowReviewWait - time.Nanosecond)
	source.pollHook = func() { current = completedAt.Add(shadowReviewWait) }
	complete, err = w.reconcileShadowReview(ctx, task, binding, checkpoint, nil, instructions, routed)
	if err != nil || !complete || gotClass != domain.ReviewFailureTransient {
		t.Fatalf("expired shadow review = complete %v, class %q, err %v", complete, gotClass, err)
	}
	if source.teardowns != 1 || source.requested {
		t.Fatalf("expired shadow teardown = %d, requested %v", source.teardowns, source.requested)
	}
	latest, _, err := w.latestReviewState(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.InvocationID != routed.InvocationID {
		t.Fatalf("routed review changed by expired shadow: %#v", latest)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		records, err := tx.ListShadowReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			t.Fatalf("late shadow records = %#v", records)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResolvedShadowReviewRateFallsBackClampsAndSelectsStably(t *testing.T) {
	policy, err := domain.NewResolvedPolicy("run-rate", []domain.PolicyKey{{
		Key: shadowReviewRatePolicyKey, Value: "1.5",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: shadowTestDigest("a")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvedShadowReviewRate(policy, 0.2); got != 1 {
		t.Fatalf("clamped rate = %v", got)
	}
	if !shadowReviewSelected("run-rate", 3, 1) || shadowReviewSelected("run-rate", 3, 0) {
		t.Fatal("rate boundary selection failed")
	}
	first := shadowReviewSelected("run-rate", 3, 0.5)
	if second := shadowReviewSelected("run-rate", 3, 0.5); first != second {
		t.Fatalf("stable selection changed: %v then %v", first, second)
	}
	invalid := policy
	invalid.Keys[0].Value = "not-a-rate"
	if got := resolvedShadowReviewRate(invalid, 0.2); got != 0.2 {
		t.Fatalf("invalid rate fallback = %v", got)
	}
}

func TestShadowReviewAttentionDecisionRequiresBoundTerminalCommand(t *testing.T) {
	decidedAt := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	baseItem := domain.AttentionItem{
		ID: "item-shadow-decision", Type: domain.AttentionReviewDispute,
		Status:    domain.StatusResolved,
		DecidedAt: &decidedAt, ItemVersion: 2, PRHeadSHA: strings.Repeat("a", 40),
		RequestedDecision: []domain.Action{
			domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop,
		},
		ArtifactDigests: []domain.Digest{shadowTestDigest("d")},
	}
	command := func(action domain.Action) domain.Command {
		return domain.Command{
			CommandID: "command-" + string(action), DeviceID: "device-shadow",
			ItemID: baseItem.ID, ItemVersion: 1, PRHeadSHA: baseItem.PRHeadSHA,
			ArtifactDigests: baseItem.ArtifactDigests, Action: action,
		}
	}
	tests := []struct {
		name     string
		item     domain.AttentionItem
		commands []domain.Command
		want     domain.Action
		wantErr  bool
	}{
		{name: "approve", item: baseItem, commands: []domain.Command{command(domain.ActionApprove)}, want: domain.ActionApprove},
		{name: "stop", item: baseItem, commands: []domain.Command{command(domain.ActionStop)}, want: domain.ActionStop},
		{
			name: "discussion before approval", item: baseItem,
			commands: []domain.Command{command(domain.ActionDiscuss), command(domain.ActionApprove)},
			want:     domain.ActionApprove,
		},
		{name: "resolved without terminal command", item: baseItem, wantErr: true},
		{
			name: "multiple terminal commands", item: baseItem,
			commands: []domain.Command{command(domain.ActionApprove), command(domain.ActionStop)}, wantErr: true,
		},
		{name: "discussion only", item: baseItem, commands: []domain.Command{command(domain.ActionDiscuss)}, wantErr: true},
		{name: "foreign action", item: baseItem, commands: []domain.Command{command(domain.Action("adjudicate"))}, wantErr: true},
		{name: "open", item: func() domain.AttentionItem {
			item := baseItem
			item.Status = domain.StatusOpen
			item.DecidedAt = nil
			return item
		}()},
		{name: "open with terminal", item: func() domain.AttentionItem {
			item := baseItem
			item.Status = domain.StatusOpen
			item.DecidedAt = nil
			return item
		}(), commands: []domain.Command{command(domain.ActionApprove)}, wantErr: true},
		{name: "missing decision timestamp", item: func() domain.AttentionItem { item := baseItem; item.DecidedAt = nil; return item }(), commands: []domain.Command{command(domain.ActionApprove)}, wantErr: true},
		{name: "stale binding", item: baseItem, commands: []domain.Command{func() domain.Command {
			stale := command(domain.ActionApprove)
			stale.PRHeadSHA = strings.Repeat("b", 40)
			return stale
		}()}, wantErr: true},
		{name: "routed action set", item: func() domain.AttentionItem {
			item := baseItem
			item.RequestedDecision = []domain.Action{domain.Action("adjudicate"), domain.ActionDiscuss, domain.ActionStop}
			return item
		}(), commands: []domain.Command{command(domain.ActionStop)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := shadowReviewAttentionDecision(tt.item, tt.commands)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("decision = %q, %v, want %q, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestShadowReviewAttentionBindsCurrentTaskAndRecord(t *testing.T) {
	runID := domain.RunID("run-shadow-binding")
	headSHA := strings.Repeat("a", 40)
	record := domain.ShadowReviewRecord{
		InvocationID: "shadow-invocation", RunID: runID, HeadSHA: headSHA,
	}
	task := productionPublicationTask{
		RunID: runID, ProjectID: "project-shadow-binding", HeadSHA: headSHA,
	}
	item := domain.AttentionItem{
		ID: shadowReviewAttentionItemID(record.InvocationID), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		PRHeadSHA: headSHA,
	}
	if !shadowReviewAttentionBindsTask(item, task, record) {
		t.Fatal("exact shadow attention binding was rejected")
	}
	tests := []struct {
		name   string
		mutate func(*domain.AttentionItem, *productionPublicationTask, *domain.ShadowReviewRecord)
	}{
		{name: "foreign item id", mutate: func(item *domain.AttentionItem, _ *productionPublicationTask, _ *domain.ShadowReviewRecord) {
			item.ID = "foreign-item"
		}},
		{name: "foreign project", mutate: func(item *domain.AttentionItem, _ *productionPublicationTask, _ *domain.ShadowReviewRecord) {
			item.ProjectID = "foreign-project"
		}},
		{name: "foreign subject", mutate: func(item *domain.AttentionItem, _ *productionPublicationTask, _ *domain.ShadowReviewRecord) {
			item.Subject.ID = "foreign-run"
		}},
		{name: "foreign item head", mutate: func(item *domain.AttentionItem, _ *productionPublicationTask, _ *domain.ShadowReviewRecord) {
			item.PRHeadSHA = strings.Repeat("b", 40)
		}},
		{name: "foreign record run", mutate: func(_ *domain.AttentionItem, _ *productionPublicationTask, record *domain.ShadowReviewRecord) {
			record.RunID = "foreign-run"
		}},
		{name: "foreign task head", mutate: func(_ *domain.AttentionItem, task *productionPublicationTask, _ *domain.ShadowReviewRecord) {
			task.HeadSHA = strings.Repeat("b", 40)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotItem, gotTask, gotRecord := item, task, record
			tt.mutate(&gotItem, &gotTask, &gotRecord)
			if shadowReviewAttentionBindsTask(gotItem, gotTask, gotRecord) {
				t.Fatal("foreign shadow attention binding was accepted")
			}
		})
	}
}

func shadowTestDigest(seed string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(seed, 64))
}
