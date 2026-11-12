package admissionauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// testRunEvidenceMetadata is valid run-sourced evidence metadata, a copy of
// cmd/freesided's fixture helper.
func testRunEvidenceMetadata(sizeBytes int64) domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType:    domain.EvidenceMediaApplicationJSON,
		SizeBytes:    sizeBytes,
		CreatedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Source:       domain.EvidenceSourceRun,
		Availability: domain.EvidenceAvailable,
	}
}

type fixedProductionCommitAuthorResolver struct {
	identity publish.AppBotIdentity
	err      error
}

// countingProductionCommitAuthorResolver's fields are exported so the
// package's external tests can set and read them through an alias.
type countingProductionCommitAuthorResolver struct {
	Identity        publish.AppBotIdentity
	ResolveCalls    int
	RevalidateCalls int
}

func (r *countingProductionCommitAuthorResolver) Resolve(
	context.Context,
	string,
) (publish.AppBotIdentity, error) {
	r.ResolveCalls++
	return r.Identity, nil
}

func (r *countingProductionCommitAuthorResolver) Revalidate(
	context.Context,
	string,
) (publish.AppBotIdentity, error) {
	r.RevalidateCalls++
	return r.Identity, nil
}

func (r fixedProductionCommitAuthorResolver) Resolve(
	context.Context,
	string,
) (publish.AppBotIdentity, error) {
	return r.identity, r.err
}

func (r fixedProductionCommitAuthorResolver) Revalidate(
	context.Context,
	string,
) (publish.AppBotIdentity, error) {
	return r.identity, r.err
}

// submissionSpecification is the task `freesided submit` tests submit; the
// fallback commit message derives its subject, "Task", from the heading.
const submissionSpecification = "# Task\n\nImplement the thing.\n"

// submissionFixture is the state a `freesided submit` left for the
// admission authority's tests, seeded through the engine so the tests don't
// depend on the submit command: the specification bytes and both artifacts,
// the private specification run with its outbox marker, and the policy keys
// production runs resolve from. Its fields are exported so the package's
// external tests can read them through an alias.
type submissionFixture struct {
	Store         *store.Store
	Blobs         *signet.BlobStore
	ProjectID     domain.ProjectID
	SpecArtifact  domain.Artifact
	Keys          []domain.PolicyKey
	PolicyID      domain.ArtifactID
	Specification engine.SpecificationRun
}

// newSubmissionFixture opens a store under opts and seeds one submission
// the way `freesided submit` applies it: bytes land in the blob store, the
// artifacts register write-once, and the specification run converges in
// the same transaction.
func newSubmissionFixture(t *testing.T, opts store.Options) submissionFixture {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	st := storetest.Open(t, filepath.Join(root, "freeside.db"), opts)
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	digest := func(body []byte) domain.Digest { return domain.Digest(contentaddr.Sum(body)) }
	put := func(body []byte) domain.Digest {
		t.Helper()
		d := digest(body)
		if _, err := blobs.Put(d, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	provenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride,
		Digest: domain.Digest("sha256:" + string(bytes.Repeat([]byte("ab"), 32))),
	}
	// The keys are in canonical order, so their JSON is the resolved policy's
	// digest input and the policy artifact names the resolved policy.
	keys := []domain.PolicyKey{
		{Key: "budgets.stage_active_time", Value: "1h", Provenance: provenance},
		{Key: "gates.spec_approval", Value: "true", Provenance: provenance},
		{Key: "paths", Value: "daemon/**", Provenance: provenance},
		{Key: "research.allowlist", Value: "example.com", Provenance: provenance},
		{Key: "research.max_response_bytes", Value: "1024", Provenance: provenance},
		{Key: "specification.max_iterations", Value: "4", Provenance: provenance},
		{Key: "waiting.spec_approval_attention_after", Value: "1m", Provenance: provenance},
	}
	source := []byte(submissionSpecification)
	policyBody, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, policyDigest := put(source), put(policyBody)
	specArtifact, err := engine.SubmissionArtifact(
		domain.ArtifactKindSpecification, sourceDigest, domain.EvidenceMediaTextMarkdown, int64(len(source)))
	if err != nil {
		t.Fatal(err)
	}
	policyArtifact, err := engine.SubmissionArtifact(
		domain.ArtifactKindPolicy, policyDigest, domain.EvidenceMediaApplicationJSON, int64(len(policyBody)))
	if err != nil {
		t.Fatal(err)
	}
	publication := engine.ProductionPublication{
		Title: "Test the task", Body: "## Why\n\nCloses #123.\n",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	const (
		identity  = "cli:authority-fixture"
		projectID = domain.ProjectID("proj-authority")
	)
	workUnit := &domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged, DeclaredPaths: engine.DeclaredPathScope(keys),
	}
	workUnitDigest := digest([]byte(`{"completion_criterion":"bound_pr_merged"}`))
	implementationRunID := engine.ManualSubmissionRunID(
		identity, projectID, sourceDigest, policyDigest, digest(publicationBody), workUnitDigest)
	specificationRunID, err := engine.SpecificationRunIDForImplementation(implementationRunID)
	if err != nil {
		t.Fatal(err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(implementationRunID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := domain.NewResolvedPolicy(specificationRunID, keys)
	if err != nil {
		t.Fatal(err)
	}
	var specification engine.SpecificationRun
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := engine.RegisterSubmissionArtifact(ctx, tx, specArtifact); err != nil {
			return err
		}
		if err := engine.RegisterSubmissionArtifact(ctx, tx, policyArtifact); err != nil {
			return err
		}
		var err error
		specification, err = engine.SubmitSpecificationRunTx(ctx, tx, engine.SpecificationRunSpec{
			ManualSubmission: &domain.ManualSubmission{
				Identity: identity, ProjectID: projectID, SourceArtifactID: specArtifact.ID,
				SourceDigest: sourceDigest, RequestDigest: digest([]byte(identity)),
				ImplementationRunID: implementationRunID,
			},
			SpecificationRunID: specificationRunID, ImplementationRunID: implementationRunID,
			ProjectID: projectID, SourceArtifactID: specArtifact.ID, SourceBytes: source,
			PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolved, Publication: publication,
			PublicationDigest: digest(publicationBody),
			WorkUnit:          workUnit, CampaignID: campaignID, AttemptNumber: 1,
		})
		return err
	}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	return submissionFixture{
		Store: st, Blobs: blobs, ProjectID: projectID, SpecArtifact: specArtifact,
		Keys: keys, PolicyID: policyArtifact.ID, Specification: specification,
	}
}

// SubmitProduction submits a production run of the fixture's specification
// under the fixture's policy keys, as an approved specification would.
func (f submissionFixture) SubmitProduction(
	t *testing.T, runID domain.RunID, publication engine.ProductionPublication,
) engine.ProductionRun {
	t.Helper()
	policy, err := domain.NewResolvedPolicy(runID, f.Keys)
	if err != nil {
		t.Fatal(err)
	}
	production, err := engine.SubmitProductionRun(t.Context(), f.Store, engine.ProductionRunSpec{
		RunID: runID, ProjectID: f.ProjectID,
		SpecArtifactID: f.SpecArtifact.ID, PolicyArtifactID: f.PolicyID,
		ResolvedPolicy: policy, Publication: publication,
	})
	if err != nil {
		t.Fatalf("submit production run %s: %v", runID, err)
	}
	return production
}

func TestAuthorityAuthenticatesInvocationAuthors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fixture := newSubmissionFixture(t, store.Options{})
	st, first := fixture.Store, fixture.Specification
	authorityRunID := domain.RunID("authority-" + string(first.ImplementationRunID))
	authoritySubmission := fixture.SubmitProduction(t, authorityRunID, engine.ProductionPublication{
		Title: "Test the task", Body: "## Why\n\nCloses #123.\n",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	})
	otherAuthoritySubmission := fixture.SubmitProduction(t, "authority-other", engine.ProductionPublication{
		Title: "Describe another outcome", Body: "## Why\n\nCloses #456.\n",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	})
	authority := Authority{
		Store: st,
		CommitAuthors: fixedProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
			AppSlug: "freeside-test", BotUserID: 12345,
		}},
	}
	commitAuthorErr := errors.New("commit author resolution must not run for specification")
	authority.CommitAuthors = fixedProductionCommitAuthorResolver{err: commitAuthorErr}
	if err := authority.authenticateInvocationStart(ctx, first.SpecificationInvocationID,
		domain.ExecutionAdmission{
			RunID: first.Run.ID, StageID: first.SpecificationStageID,
			OperatingMode: domain.ModeUnattended,
		}, "example/repo"); err != nil {
		t.Fatalf("authenticate specification start without publication author: %v", err)
	}
	if err := authority.authenticateInvocationStart(ctx, first.SpecificationInvocationID,
		domain.ExecutionAdmission{
			RunID: authorityRunID, StageID: first.SpecificationStageID,
			OperatingMode: domain.ModeUnattended,
		}, "example/repo"); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("authenticate specification start with foreign run = %v, want ErrParentKeyMismatch", err)
	}
	specificationAdmission := domain.ExecutionAdmission{
		RunID: first.Run.ID, StageID: first.SpecificationStageID,
		OperatingMode: domain.ModeUnattended,
	}
	if author, production, err := authority.invocationImportAuthor(
		ctx, first.SpecificationInvocationID, specificationAdmission, "example/repo",
	); err != nil || production || author != (engine.ProductionCommitAuthor{}) {
		t.Fatalf("authenticate specification import author = %#v, production=%t, err=%v", author, production, err)
	}
	if author, production, err := authority.invocationImportRecordAuthor(
		ctx, first.SpecificationInvocationID, specificationAdmission,
	); err != nil || production || author != (engine.ProductionCommitAuthor{}) {
		t.Fatalf("authenticate specification replay author = %#v, production=%t, err=%v", author, production, err)
	}
	// A specification invocation imports under the specification finding profile,
	// so investigation debris does not definitively fail it (#768). Both
	// ImportOptions and ImportOptionsRecord set it through this one helper, so
	// the live import and its terminal replay reconstruct the same profile.
	var specificationOpts importer.Options
	if err := authority.applySpecificationFindingProfile(
		ctx, first.SpecificationInvocationID, specificationAdmission, &specificationOpts,
	); err != nil {
		t.Fatalf("apply specification finding profile: %v", err)
	}
	if specificationOpts.Policy.FindingProfile == nil ||
		*specificationOpts.Policy.FindingProfile != importer.FindingProfileSpecification {
		t.Fatalf("specification finding profile = %v, want specification",
			specificationOpts.Policy.FindingProfile)
	}
	// A production invocation is left with a nil profile (the default
	// publish-strict, omitted on the wire), so its recorded ImportOptions and
	// publication task payload are byte-identical.
	var productionOpts importer.Options
	if err := authority.applySpecificationFindingProfile(
		ctx, authoritySubmission.InvocationID,
		domain.ExecutionAdmission{RunID: authorityRunID, OperatingMode: domain.ModeUnattended},
		&productionOpts,
	); err != nil {
		t.Fatalf("apply finding profile for production invocation: %v", err)
	}
	if productionOpts.Policy.FindingProfile != nil {
		t.Fatalf("production finding profile = %v, want nil (publish-strict default)",
			productionOpts.Policy.FindingProfile)
	}
	authority.CommitAuthors = fixedProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
		AppSlug: "freeside-test", BotUserID: 12345,
	}}
	author, production, err := authority.authenticateProductionCommitAuthor(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "example/repo",
	)
	if err != nil {
		t.Fatalf("read durable production author: %v", err)
	}
	if !production || author.Name() != "freeside-test[bot]" ||
		author.Email() != "12345+freeside-test[bot]@users.noreply.github.com" {
		t.Fatalf("durable production author = %#v, production=%v", author, production)
	}
	countingResolver := &countingProductionCommitAuthorResolver{Identity: publish.AppBotIdentity{
		AppSlug: "freeside-test", BotUserID: 12345,
	}}
	authority.CommitAuthors = countingResolver
	authority.StartAuthors = NewCommitAuthorCache()
	for range 2 {
		if _, _, err := authority.authenticateProductionCommitAuthorForStart(
			ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "example/repo",
		); err != nil {
			t.Fatalf("authenticate cached production start author: %v", err)
		}
	}
	if countingResolver.ResolveCalls != 1 || countingResolver.RevalidateCalls != 0 {
		t.Fatalf("stable start made %d resolve and %d revalidate calls, want 1 and 0",
			countingResolver.ResolveCalls, countingResolver.RevalidateCalls)
	}
	if _, _, err := authority.authenticateProductionCommitAuthorForStart(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "another/repo",
	); err != nil {
		t.Fatalf("re-authenticate changed start binding: %v", err)
	}
	authority.StartAuthors.forget(authoritySubmission.InvocationID)
	if _, _, err := authority.authenticateProductionCommitAuthorForStart(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "another/repo",
	); err != nil {
		t.Fatalf("re-authenticate forgotten start binding: %v", err)
	}
	if countingResolver.ResolveCalls != 3 {
		t.Fatalf("changed and forgotten starts made %d resolve calls, want 3", countingResolver.ResolveCalls)
	}
	if _, _, err := authority.authenticateProductionCommitAuthorRevalidated(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "another/repo",
	); err != nil {
		t.Fatalf("revalidate production author before import: %v", err)
	}
	if countingResolver.RevalidateCalls != 1 {
		t.Fatalf("import boundary made %d revalidate calls, want 1", countingResolver.RevalidateCalls)
	}
	countingResolver.Identity = publish.AppBotIdentity{AppSlug: "another-app", BotUserID: 67890}
	if _, _, err := authority.authenticateProductionCommitAuthorForStart(
		ctx, otherAuthoritySubmission.InvocationID, domain.ModeUnattended, "example/repo",
	); !errors.Is(err, publish.ErrAppBotIdentityMismatch) {
		t.Fatalf("new invocation under changed App binding = %v, want ErrAppBotIdentityMismatch", err)
	}
	authority.CommitAuthors = fixedProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
		AppSlug: "another-app", BotUserID: 67890,
	}}
	if _, _, err := authority.authenticateProductionCommitAuthor(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "example/repo",
	); !errors.Is(err, publish.ErrAppBotIdentityMismatch) {
		t.Fatalf("mismatched selected App author = %v, want ErrAppBotIdentityMismatch", err)
	}
	legacyID := domain.InvocationID("inv-implement-run-legacy-author")
	legacyPayload := []byte(`{"invocation_id":"inv-implement-run-legacy-author","run_id":"run-legacy-author","stage_id":"implement-run-legacy-author"}`)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, string(legacyID), engine.KindProductionInvocationRequested, legacyPayload)
		return err
	}); err != nil {
		t.Fatalf("seed legacy production marker: %v", err)
	}
	if author, production, err := authority.authenticateProductionCommitAuthor(
		ctx, legacyID, domain.ModeUnattended, "example/repo",
	); err != nil || production || author != (engine.ProductionCommitAuthor{}) {
		t.Fatalf("legacy production author = %#v, production=%v, err=%v", author, production, err)
	}
	remediationID, remediationAdmission := seedRemediationAuthorityFixture(
		t, ctx, st, authorityRunID,
	)
	remediationResolver := &countingProductionCommitAuthorResolver{Identity: publish.AppBotIdentity{
		AppSlug: "freeside-test", BotUserID: 12345,
	}}
	authority.CommitAuthors = remediationResolver
	authority.StartAuthors = NewCommitAuthorCache()
	if err := authority.authenticateInvocationStart(
		ctx, remediationID, remediationAdmission, "example/repo",
	); err != nil {
		t.Fatalf("authenticate remediation start: %v", err)
	}
	wantRemediationAuthor := engine.ProductionCommitAuthor{
		AppSlug: "freeside-test", BotUserID: 12345,
	}
	author, production, err = authority.invocationImportAuthor(
		ctx, remediationID, remediationAdmission, "example/repo",
	)
	if err != nil || !production || author != wantRemediationAuthor {
		t.Fatalf("authenticate remediation live import = %#v, production=%t, err=%v",
			author, production, err)
	}
	author, production, err = authority.invocationImportRecordAuthor(
		ctx, remediationID, remediationAdmission,
	)
	if err != nil || !production || author != wantRemediationAuthor {
		t.Fatalf("authenticate remediation replay import = %#v, production=%t, err=%v",
			author, production, err)
	}
	if remediationResolver.ResolveCalls != 1 || remediationResolver.RevalidateCalls != 1 {
		t.Fatalf("remediation author authentication made %d resolve and %d revalidate calls, want 1 each",
			remediationResolver.ResolveCalls, remediationResolver.RevalidateCalls)
	}
}

func seedRemediationAuthorityFixture(
	t *testing.T,
	ctx context.Context,
	st *store.Store,
	runID domain.RunID,
) (domain.InvocationID, domain.ExecutionAdmission) {
	t.Helper()
	digest := func(body string) domain.Digest {
		return domain.Digest(contentaddr.Sum([]byte(body)))
	}
	at := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	baseSHA := strings.Repeat("1", 40)
	headSHA := strings.Repeat("2", 40)
	reviewID := engine.ProductionReviewInvocationID(runID, 1)
	finding := domain.Finding{
		ID: "finding-remediation-authority", RunID: runID, Source: "codex_local",
		Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "repair the trust boundary", RawText: "repair the trust boundary", CreatedAt: at,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: reviewID, RunID: runID, Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, runID, reviewID),
		ConfigurationDigest: digest("review configuration"),
		InstructionDigest:   digest("review instructions"),
		BaseSHA:             baseSHA, HeadSHA: headSHA, CompletedAt: at,
		CompletionEvidence: digest("review completion"), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	compatibility := domain.CompatibilityAllowed
	entry, err := domain.NewEngineAdjudicationEntry(
		finding.ID, domain.GoalRequired, &compatibility, domain.RouteRemediate,
		"remediate", []string{finding.Location.String()}, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var run domain.Run
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	adjudication, err := domain.NewFindingAdjudication(
		runID, 1, run.SpecDigest, record.InstructionDigest, run.PolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, "", at,
	)
	if err != nil {
		t.Fatal(err)
	}
	remediationID := domain.InvocationID("inv-remediate-1-" + string(runID))
	stageID := domain.StageID("remediate-1-" + string(runID))
	inputArtifactID := domain.ArtifactID("remediation-input-1-" + string(runID))
	inputDigest := digest("remediation input envelope")
	inputArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: inputArtifactID, Type: domain.ArtifactKindEvidence, Digest: inputDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: reviewID,
			HeadBinding: domain.HeadBound, SourceHeadSHA: headSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(1),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := func() (domain.AgentInvocation, error) {
		var invocation domain.AgentInvocation
		err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			invocation, err = tx.GetAgentInvocation(ctx, domain.InvocationID("inv-implement-"+string(runID)))
			return err
		})
		return invocation, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	remediationInvocation, err := domain.NewAgentInvocation(
		remediationID, []domain.ArtifactID{initial.InputIDs[0], inputArtifactID}, nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Version             string              `json:"version"`
		InvocationID        domain.InvocationID `json:"invocation_id"`
		RunID               domain.RunID        `json:"run_id"`
		StageID             domain.StageID      `json:"stage_id"`
		Round               int                 `json:"round"`
		ReviewInvocationID  domain.InvocationID `json:"review_invocation_id"`
		AdjudicationDigest  domain.Digest       `json:"adjudication_digest"`
		InputArtifactID     domain.ArtifactID   `json:"input_artifact_id"`
		InputArtifactDigest domain.Digest       `json:"input_artifact_digest"`
		BaseSHA             string              `json:"base_sha"`
		HeadSHA             string              `json:"head_sha"`
		FindingIDs          []domain.FindingID  `json:"finding_ids"`
	}{
		Version: "freeside.remediation-request/v1", InvocationID: remediationID,
		RunID: runID, StageID: stageID, Round: 1, ReviewInvocationID: reviewID,
		AdjudicationDigest: adjudication.Digest, InputArtifactID: inputArtifactID,
		InputArtifactDigest: inputDigest, BaseSHA: baseSHA, HeadSHA: headSHA,
		FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Stages = append(run.Stages, domain.Stage{ID: stageID, RunID: runID, Name: "implement"})
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, inputArtifact); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, remediationInvocation); err != nil {
			return err
		}
		_, _, err := tx.EnqueueOutbox(
			ctx, string(remediationID), engine.KindRemediationInvocationRequested, payload,
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return remediationID, domain.ExecutionAdmission{
		RunID: runID, StageID: stageID, OperatingMode: domain.ModeUnattended,
	}
}

func TestAuthorityDerivesFallbackCommitMessage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fixture := newSubmissionFixture(t, store.Options{})
	st, blobs := fixture.Store, fixture.Blobs
	productionRunID := domain.RunID("fallback-" + string(fixture.Specification.ImplementationRunID))
	production := fixture.SubmitProduction(t, productionRunID, engine.ProductionPublication{
		Title: "Test the task", Body: "## Why\n\nCloses #123.\n",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	})
	authority := Authority{Store: st, Blobs: blobs}
	admission := domain.ExecutionAdmission{
		RunID: production.Run.ID, SpecDigest: fixture.SpecArtifact.Digest,
	}
	got, err := authority.fallbackCommitMessage(ctx, admission, importer.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if subject, _, _ := strings.Cut(got, "\n"); subject != "Task" {
		t.Fatalf("undeclared subject = %q, want %q", subject, "Task")
	}

	issue := 123
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &issue,
		DeclaredPaths:       []string{"daemon/**"},
	}, production.Run.ID, fixture.ProjectID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
	got, err = authority.fallbackCommitMessage(ctx, admission, importer.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if subject, _, _ := strings.Cut(got, "\n"); subject != "Task (#123)" {
		t.Fatalf("issue-bound subject = %q, want %q", subject, "Task (#123)")
	}
	reconstructed, err := authority.fallbackCommitMessage(ctx, admission, importer.Policy{})
	if err != nil || reconstructed != got {
		t.Fatalf("reconstructed message = %q, %v; want byte-identical %q", reconstructed, err, got)
	}
}

func TestAuthorityRejectsMissingSpecificationMarker(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	authority := Authority{Store: st}
	specification, err := authority.authenticateSpecificationInvocation(
		t.Context(), "inv-specify-run-missing-2", domain.ExecutionAdmission{
			RunID: "run-missing", StageID: "specify-run-missing",
		},
	)
	if !specification || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing specification marker = specification %t, error %v; want fail-closed ErrNotFound",
			specification, err)
	}
}

func TestAttendedImportUsesCurrentActiveTrustProfile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 6, 15, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, now); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, profile.Repo, profile.ProfileDigest, now)
	}); err != nil {
		t.Fatal(err)
	}
	authority := Authority{Store: st}
	admission := domain.ExecutionAdmission{
		ID: "sha256:attended-admission", OperatingMode: domain.ModeAttendedDev,
		Base: domain.BaseRevision{Repo: profile.Repo, RepositoryID: profile.RepositoryID},
	}
	got, err := authority.importTrustProfile(ctx, admission)
	if err != nil {
		t.Fatalf("attended import profile: %v", err)
	}
	if got.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("attended import profile = %s, want %s", got.ProfileDigest, profile.ProfileDigest)
	}
	admission.OperatingMode = domain.ModeUnattended
	digest := profile.ProfileDigest
	admission.TrustProfileDigest = &digest
	got, err = authority.importTrustProfile(ctx, admission)
	if err != nil {
		t.Fatalf("bound unattended import profile: %v", err)
	}
	if got.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("unattended import profile = %s, want %s", got.ProfileDigest, profile.ProfileDigest)
	}
	admission.TrustProfileDigest = nil
	if _, err := authority.importTrustProfile(ctx, admission); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("unbound unattended import profile = %v, want ErrEmptyField", err)
	}
}

// An attended admission in a repository with no approved profile starts with
// the built-in patterns, since import, not start, waits for a profile; one
// bound to a profile the store lacks still fails.
func TestAttendedProtectedPatternsWithoutProfile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	authority := Authority{Store: st}
	admission := domain.ExecutionAdmission{
		ID: "sha256:attended-admission", OperatingMode: domain.ModeAttendedDev,
		Base: domain.BaseRevision{Repo: "example/repo", RepositoryID: 44},
	}
	spec := exec.StartSpec{Base: admission.Base}
	got, err := authority.protectedPatterns(ctx, admission, spec)
	if err != nil {
		t.Fatalf("attended without a profile: %v", err)
	}
	if want := (importer.Policy{}).PublishBlockingPatterns(); !slices.Equal(got, want) || len(want) == 0 {
		t.Fatalf("patterns = %q, want the built-in list %q", got, want)
	}
	missing := domain.Digest("sha256:" + strings.Repeat("9", 64))
	admission.TrustProfileDigest = &missing
	if _, err := authority.protectedPatterns(ctx, admission, spec); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bound profile missing = %v, want ErrNotFound", err)
	}
}
