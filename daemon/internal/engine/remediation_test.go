package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type remediationReviewFixture struct {
	workflow *productionPublicationWorkflow
	task     productionPublicationTask
	prior    domain.Finding
	priors   []domain.Finding
	current  domain.ReviewRecord
}

type remediationArtifactStore struct {
	body     []byte
	openErr  error
	readErr  error
	closeErr error
}

func (s remediationArtifactStore) Put(domain.Digest, io.Reader) (bool, error) {
	return false, errors.New("unexpected artifact write")
}

func (s remediationArtifactStore) Open(domain.Digest) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	if s.readErr != nil {
		return remediationFailingReadCloser{s.readErr}, nil
	}
	if s.closeErr != nil {
		return remediationCloseErrorReader{
			Reader: bytes.NewReader(s.body), err: s.closeErr,
		}, nil
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

type remediationFailingReadCloser struct {
	err error
}

func (r remediationFailingReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (remediationFailingReadCloser) Close() error               { return nil }

type remediationCloseErrorReader struct {
	io.Reader
	err error
}

func (r remediationCloseErrorReader) Close() error { return r.err }

func TestActiveRemediationStageUsesHighestDurableTransition(t *testing.T) {
	runID := domain.RunID("run-active-remediation")
	run := domain.Run{ID: runID, Stages: []domain.Stage{{
		ID: domain.StageID("implement-" + string(runID)), RunID: runID, Name: productionStageName,
	}}}
	if _, _, active, err := activeRemediationStage(run); err != nil || active {
		t.Fatalf("no remediation = %t, %v", active, err)
	}
	run.Stages = append(run.Stages,
		domain.Stage{ID: remediationStageID(runID, 1), RunID: runID, Name: productionStageName},
		domain.Stage{ID: remediationStageID(runID, 3), RunID: runID, Name: productionStageName},
		domain.Stage{ID: remediationStageID(runID, 2), RunID: runID, Name: productionStageName},
	)
	stage, round, active, err := activeRemediationStage(run)
	if err != nil || !active || round != 3 || stage.ID != remediationStageID(runID, 3) {
		t.Fatalf("active remediation = %#v, %d, %t, %v", stage, round, active, err)
	}
}

func TestAuthenticateProductionRunTransitionLoadsCurrentRunWithinSnapshot(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	runID := domain.RunID("run-current-remediation-transition")
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-current-remediation-transition",
			SpecDigest: adjudicationDigest("a"), PolicyDigest: adjudicationDigest("b"),
			Stages: []domain.Stage{{
				ID: remediationStageID(runID, 2), RunID: runID,
				Name: productionStageName, Attempts: []domain.Attempt{},
			}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := authenticateProductionRunTransition(ctx, tx, runID)
		return err
	}); !errors.Is(err, errProductionMarkerUnreadable) {
		t.Fatalf("current round without its marker = %v, want remediation hold", err)
	}
}

func TestRemediationRequestRejectsNondeterministicInputArtifactID(t *testing.T) {
	runID := domain.RunID("run-remediation-request")
	request := remediationInvocationRequest{
		Version: remediationRequestVersion, InvocationID: remediationInvocationID(runID, 1),
		RunID: runID, StageID: remediationStageID(runID, 1), Round: 1,
		ReviewInvocationID:  ProductionReviewInvocationID(runID, 1),
		AdjudicationDigest:  adjudicationDigest("1"),
		InputArtifactID:     "foreign-remediation-input",
		InputArtifactDigest: adjudicationDigest("2"),
		BaseSHA:             strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		FindingIDs: []domain.FindingID{"finding-1"},
	}
	if _, err := encodeRemediationRequest(request); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("nondeterministic remediation input id = %v, want ErrParentKeyMismatch", err)
	}
}

func TestRemediationCandidatePatchPreservesNonUTF8Bytes(t *testing.T) {
	repo := t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	want := []byte("after \xfe\n")
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("before \xff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, repo, nil, "add", "candidate.txt")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "base")
	baseSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, repo, nil, "add", "candidate.txt")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "candidate")
	headSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	runRemediationGit(t, repo, nil, "reset", "-q", "--hard", baseSHA)

	patch, err := remediationCandidatePatch(t.Context(), t.TempDir(), repo, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.Valid(patch) || !bytes.Contains(patch, []byte{0xfe}) {
		t.Fatalf("candidate patch did not retain the invalid UTF-8 byte: %x", patch)
	}
	encoded, err := json.Marshal(remediationInput{CandidatePatchBase64: patch})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(encoded) || bytes.Contains(encoded, []byte("\xef\xbf\xbd")) {
		t.Fatalf("encoded remediation input is not lossless UTF-8 JSON: %x", encoded)
	}
	var decoded remediationInput
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.CandidatePatchBase64, patch) {
		t.Fatal("candidate patch changed across the JSON/base64 round trip")
	}
	runRemediationGit(t, repo, decoded.CandidatePatchBase64, "apply", "--binary", "-")
	got, err := os.ReadFile(filepath.Join(repo, "candidate.txt")) //nolint:gosec // G304: test-owned fixture path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("applied candidate bytes = %x, want %x", got, want)
	}
}

func runRemediationGit(t *testing.T, dir string, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := osexec.Command("git", args...) //nolint:gosec // G204: test-owned repository and fixed fixture arguments
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=1787500000 +0000", "GIT_COMMITTER_DATE=1787500000 +0000",
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

func TestMalformedRemediationMarkerIsQuarantinedPerRun(t *testing.T) {
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runIDs := []domain.RunID{"a-malformed-remediation", "z-malformed-remediation"}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, runID := range runIDs {
			if err := tx.PutRun(ctx, domain.Run{
				ID: runID, ProjectID: "project-1",
				SpecDigest: adjudicationDigest("1"), PolicyDigest: adjudicationDigest("2"),
				Stages: []domain.Stage{{
					ID: remediationStageID(runID, 1), RunID: runID,
					Name: productionStageName, Attempts: []domain.Attempt{},
				}},
			}); err != nil {
				return err
			}
			if _, _, err := tx.EnqueueOutbox(
				ctx, string(remediationInvocationID(runID, 1)),
				KindRemediationInvocationRequested, []byte("{")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		started, err := e.dispatchPendingInvocations(ctx)
		if err != nil || started != 0 {
			t.Fatalf("dispatch malformed remediation marker = %d, %v", started, err)
		}
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		for _, runID := range runIDs {
			itemID := productionQuarantineOccurrenceID(remediationMarkerQuarantinePrefix, runID, 1)
			item, err := tx.GetAttentionItem(ctx, itemID)
			if err != nil {
				return err
			}
			if item.Subject.RunID == nil || *item.Subject.RunID != runID ||
				item.Reason != remediationQuarantineUnreadable {
				return errors.New("remediation quarantine item is not bound to the malformed marker's run")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnattributableMalformedRemediationMarkerStaysLoud(t *testing.T) {
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			ctx, "not-a-remediation-key", KindRemediationInvocationRequested, []byte("{"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dispatchPendingInvocations(ctx); err == nil {
		t.Fatal("unattributable malformed remediation marker did not stay loud")
	}
}

func TestRemediationMarkerQuarantinesMissingAuthenticatedChain(t *testing.T) {
	ctx := context.Background()
	e, st := newQuarantineEngine(t, ctx)
	runID := domain.RunID("missing-remediation-chain")
	request := remediationInvocationRequest{
		Version: remediationRequestVersion, InvocationID: remediationInvocationID(runID, 1),
		RunID: runID, StageID: remediationStageID(runID, 1), Round: 1,
		ReviewInvocationID:  ProductionReviewInvocationID(runID, 1),
		AdjudicationDigest:  adjudicationDigest("1"),
		InputArtifactID:     remediationInputArtifactID(runID, 1),
		InputArtifactDigest: adjudicationDigest("2"),
		BaseSHA:             strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		FindingIDs: []domain.FindingID{"finding-1"},
	}
	payload, err := encodeRemediationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1",
			SpecDigest: adjudicationDigest("3"), PolicyDigest: adjudicationDigest("4"),
			Stages: []domain.Stage{{
				ID: request.StageID, RunID: runID, Name: productionStageName,
			}},
		}); err != nil {
			return err
		}
		_, _, err := tx.EnqueueOutbox(
			ctx, string(request.InvocationID), KindRemediationInvocationRequested, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if started, err := e.dispatchPendingInvocations(ctx); err != nil || started != 0 {
		t.Fatalf("dispatch remediation with missing chain = %d, %v", started, err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx,
			productionQuarantineOccurrenceID(remediationMarkerQuarantinePrefix, runID, 1))
		if err != nil {
			return err
		}
		if item.Reason != remediationQuarantineUnreadable {
			return fmt.Errorf("remediation quarantine reason = %q", item.Reason)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !productionQuarantineNoticeFor(
		remediationMarkerQuarantinePrefix, remediationQuarantineUnreadable,
	) {
		t.Fatal("remediation quarantine release does not recognize its own notice")
	}
}

func newRemediationReviewFixture(t *testing.T) remediationReviewFixture {
	return newRemediationReviewFixtureWithBatch(t, false)
}

func newRemediationReviewFixtureWithBatch(
	t *testing.T,
	multiple bool,
) remediationReviewFixture {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	runID := domain.RunID("run-remediation-review")
	baseSHA := strings.Repeat("1", 40)
	priorHead := strings.Repeat("2", 40)
	currentHead := strings.Repeat("3", 40)
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	prior := domain.Finding{
		ID: "finding-prior", RunID: runID, Source: "codex_local",
		Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "unsafe transition", RawText: "unsafe transition", CreatedAt: at,
	}
	priors := []domain.Finding{prior}
	if multiple {
		second := prior
		second.ID = "finding-second"
		second.Location = &domain.FindingLocation{Path: "daemon/b.go", StartLine: 2, EndLine: 2}
		second.Message = "second unsafe transition"
		second.RawText = second.Message
		priors = append(priors, second)
	}
	findingIDs := make([]domain.FindingID, len(priors))
	for index := range priors {
		findingIDs[index] = priors[index].ID
	}
	priorRecord, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(runID, 1), RunID: runID, Round: 1,
		Provider: "codex", ModelConfiguration: "test",
		ConfigurationDigest: adjudicationDigest("1"), InstructionDigest: adjudicationDigest("2"),
		CostOwner: "test", BaseSHA: baseSHA, HeadSHA: priorHead, CompletedAt: at,
		CompletionEvidence: adjudicationDigest("3"), Outcome: domain.ReviewFindings,
		FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]domain.FindingAdjudicationEntry, len(priors))
	for index := range priors {
		allowed := domain.CompatibilityAllowed
		entries[index], err = domain.NewEngineAdjudicationEntry(
			priors[index].ID, domain.GoalRequired, &allowed, domain.RouteRemediate,
			"remediate", []string{priors[index].Location.String()}, nil, nil, nil, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	adjudication, err := domain.NewFindingAdjudication(
		runID, 1, adjudicationDigest("4"), priorRecord.InstructionDigest,
		adjudicationDigest("5"), entries, "",

		at)
	if err != nil {
		t.Fatal(err)
	}
	request := remediationInvocationRequest{
		Version: remediationRequestVersion, InvocationID: remediationInvocationID(runID, 1),
		RunID: runID, StageID: remediationStageID(runID, 1), Round: 1,
		ReviewInvocationID: priorRecord.InvocationID, AdjudicationDigest: adjudication.Digest,
		InputArtifactID:     remediationInputArtifactID(runID, 1),
		InputArtifactDigest: adjudicationDigest("6"),
		BaseSHA:             baseSHA, HeadSHA: priorHead, FindingIDs: findingIDs,
	}
	payload, err := encodeRemediationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-remediation",
			SpecDigest: adjudicationDigest("4"), PolicyDigest: adjudicationDigest("5"),
		}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, priorRecord, priors); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		_, _, err := tx.EnqueueOutbox(
			ctx, string(request.InvocationID), KindRemediationInvocationRequested, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, string(request.InvocationID))
	}); err != nil {
		t.Fatal(err)
	}
	current, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: ProductionReviewInvocationID(runID, 2), RunID: runID, Round: 2,
		Provider: "codex", ModelConfiguration: "test",
		ConfigurationDigest: priorRecord.ConfigurationDigest,
		InstructionDigest:   priorRecord.InstructionDigest, CostOwner: "test",
		BaseSHA: baseSHA, HeadSHA: currentHead, CompletedAt: at.Add(time.Minute),
		CompletionEvidence: adjudicationDigest("7"), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	return remediationReviewFixture{
		workflow: &productionPublicationWorkflow{store: st}, prior: prior,
		priors: priors, current: current,
		task: productionPublicationTask{
			RunID: runID, ProjectID: "project-remediation",
			ProducingInvocationID: request.InvocationID, HeadSHA: currentHead,
			Replay: ProductionReplay{ObservedBaseSHA: baseSHA},
		},
	}
}

func TestRemediationReviewProvesFixedOnlyWhenIdentityIsAbsent(t *testing.T) {
	f := newRemediationReviewFixture(t)
	outcome, err := f.workflow.reconcileRemediationReview(
		t.Context(), f.task, f.current, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.attention != "" || outcome.dissent != nil || len(outcome.dispositions) != 1 {
		t.Fatalf("absent identity outcome = %#v", outcome)
	}
	disposition := outcome.dispositions[0]
	if disposition.FindingID != f.prior.ID || disposition.Round != 1 ||
		disposition.Disposition != domain.ReviewDispositionFixed ||
		disposition.RemediationInvocationID != f.current.InvocationID {
		t.Fatalf("fixed disposition = %#v", disposition)
	}
}

func TestRemediationReviewReemissionForcesFreshAdjudication(t *testing.T) {
	f := newRemediationReviewFixture(t)
	reemitted := f.prior
	reemitted.ID = "finding-reemitted"
	reemitted.CreatedAt = f.current.CompletedAt
	outcome, err := f.workflow.reconcileRemediationReview(
		t.Context(), f.task, f.current, []domain.Finding{reemitted}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.dispositions) != 0 || outcome.attention != "" || outcome.dissent == nil ||
		outcome.dissent.Kind != findingDissentRemediationReemitted ||
		!slices.Equal(outcome.dissent.FindingIDs, []domain.FindingID{reemitted.ID}) {
		t.Fatalf("reemitted identity outcome = %#v", outcome)
	}
}

func TestRemediatorPushbackEscalatesWithoutClaimingFixed(t *testing.T) {
	f := newRemediationReviewFixture(t)
	body, err := json.Marshal(remediatorPushback{
		Version:    remediatorPushbackVersion,
		FindingIDs: []domain.FindingID{f.prior.ID}, Reason: "the route requires an undeclared path",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: string(body)}
	claim := domain.AgentClaim{
		Label: remediatorPushbackLabel, Artifact: "pushback", Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: f.task.ProducingInvocationID,
			HeadBinding:          domain.HeadBound, SourceHeadSHA: f.task.HeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: claimTextMeta(text),
	}
	if err := claim.Validate(); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.workflow.reconcileRemediationReview(
		t.Context(), f.task, f.current, nil, []domain.AgentClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.dispositions) != 0 || outcome.dissent != nil ||
		!strings.Contains(outcome.attention, "undeclared path") {
		t.Fatalf("pushback outcome = %#v", outcome)
	}
}

func TestMalformedRemediatorPushbackFailsClosedWithoutClaimingFixed(t *testing.T) {
	f := newRemediationReviewFixture(t)
	text := domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: `{}`}
	claim := domain.AgentClaim{
		Label: remediatorPushbackLabel, Artifact: "malformed-pushback",
		Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: f.task.ProducingInvocationID,
			HeadBinding:          domain.HeadBound, SourceHeadSHA: f.task.HeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: claimTextMeta(text),
	}
	outcome, err := f.workflow.reconcileRemediationReview(
		t.Context(), f.task, f.current, nil, []domain.AgentClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.dispositions) != 0 || outcome.dissent != nil ||
		!strings.Contains(outcome.attention, "malformed") {
		t.Fatalf("malformed pushback outcome = %#v", outcome)
	}
}

func TestArtifactBackedRemediatorPushbackClassifiesDeterministicAndOperationalFailures(t *testing.T) {
	openFailure := errors.New("artifact store unavailable")
	readFailure := errors.New("artifact read interrupted")
	closeFailure := errors.New("artifact close interrupted")
	for _, test := range []struct {
		name          string
		store         remediationArtifactStore
		wantAttention string
		wantErr       error
	}{
		{
			name: "oversized body is malformed",
			store: remediationArtifactStore{
				body: bytes.Repeat([]byte("x"), domain.MaxClaimTextBytes+1),
			},
			wantAttention: "malformed",
		},
		{
			name: "digest mismatch is unauthenticated",
			store: remediationArtifactStore{
				body: []byte(`{"version":"freeside.remediator-pushback/v1","finding_ids":["finding-prior"],"reason":"blocked"}`),
			},
			wantAttention: "could not be authenticated",
		},
		{
			name: "open failure remains retryable",
			store: remediationArtifactStore{
				openErr: openFailure,
			},
			wantErr: openFailure,
		},
		{
			name: "read failure remains retryable",
			store: remediationArtifactStore{
				readErr: readFailure,
			},
			wantErr: readFailure,
		},
		{
			name: "close failure remains retryable",
			store: remediationArtifactStore{
				body: []byte("complete artifact"), closeErr: closeFailure,
			},
			wantErr: closeFailure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRemediationReviewFixture(t)
			f.workflow.artifacts = test.store
			claim := domain.AgentClaim{
				Label: remediatorPushbackLabel, Artifact: "artifact-pushback",
				Digest: adjudicationDigest("9"),
				Provenance: domain.Provenance{
					ProducerClass:        domain.ProducerAgent,
					ProducerInvocationID: f.task.ProducingInvocationID,
					HeadBinding:          domain.HeadBound,
					SourceHeadSHA:        f.task.HeadSHA,
					SensitivityClass:     domain.SensitivityNormal,
				},
				Metadata: claimMeta(domain.EvidenceMediaImagePNG),
			}
			outcome, err := f.workflow.reconcileRemediationReview(
				t.Context(), f.task, f.current, nil, []domain.AgentClaim{claim})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				if outcome.attention != "" {
					t.Fatalf("operational failure produced attention = %q", outcome.attention)
				}
				return
			}
			if !strings.Contains(outcome.attention, test.wantAttention) ||
				len(outcome.dispositions) != 0 || outcome.dissent != nil || len(outcome.claims) != 1 {
				t.Fatalf("deterministic failure outcome = %#v", outcome)
			}
		})
	}
}

func TestRemediationNoopRequiresFullPushbackCoverage(t *testing.T) {
	routed := []domain.FindingID{"finding-a", "finding-b"}
	full := &remediatorPushback{
		Version: remediatorPushbackVersion, FindingIDs: slices.Clone(routed),
		Reason: "both findings require an undeclared path",
	}
	tests := []struct {
		name     string
		pushback *remediatorPushback
		parsed   string
		want     string
	}{
		{name: "missing", want: "without a remediator-pushback claim"},
		{
			name: "partial",
			pushback: &remediatorPushback{
				Version:    remediatorPushbackVersion,
				FindingIDs: []domain.FindingID{"finding-a"}, Reason: "one route is wrong",
			},
			want: "did not cover every finding",
		},
		{name: "malformed", parsed: "The remediator-pushback claim was malformed.", want: "malformed"},
		{name: "full", pushback: full, want: "both findings require an undeclared path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := remediationNoopAttention(test.pushback, routed, test.parsed)
			if !strings.Contains(got, test.want) {
				t.Fatalf("attention = %q, want substring %q", got, test.want)
			}
		})
	}
}

func TestMixedRemediatorPushbackParksWithExactFindingIdentities(t *testing.T) {
	f := newRemediationReviewFixtureWithBatch(t, true)
	body, err := json.Marshal(remediatorPushback{
		Version:    remediatorPushbackVersion,
		FindingIDs: []domain.FindingID{f.prior.ID}, Reason: "finding A requires an undeclared path",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: string(body)}
	claim := domain.AgentClaim{
		Label: remediatorPushbackLabel, Artifact: "mixed-pushback",
		Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: f.task.ProducingInvocationID,
			HeadBinding:          domain.HeadBound, SourceHeadSHA: f.task.HeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: claimTextMeta(text),
	}
	reemitted := f.priors[1]
	reemitted.ID = "finding-second-reemitted"
	reemitted.CreatedAt = f.current.CompletedAt
	outcome, err := f.workflow.reconcileRemediationReview(
		t.Context(), f.task, f.current, []domain.Finding{reemitted},
		[]domain.AgentClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.dispositions) != 0 || outcome.dissent != nil ||
		!strings.Contains(outcome.attention, string(f.prior.ID)) ||
		strings.Contains(outcome.attention, string(reemitted.ID)) {
		t.Fatalf("mixed pushback outcome = %#v", outcome)
	}
}
