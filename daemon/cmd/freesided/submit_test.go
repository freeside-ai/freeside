package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type fixedProductionCommitAuthorResolver struct {
	identity publish.AppBotIdentity
	err      error
}

type countingProductionCommitAuthorResolver struct {
	identity        publish.AppBotIdentity
	resolveCalls    int
	revalidateCalls int
}

func (r *countingProductionCommitAuthorResolver) Resolve(
	context.Context,
	string,
) (publish.AppBotIdentity, error) {
	r.resolveCalls++
	return r.identity, nil
}

func (r *countingProductionCommitAuthorResolver) Revalidate(
	context.Context,
	string,
) (publish.AppBotIdentity, error) {
	r.revalidateCalls++
	return r.identity, nil
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

func writeSubmissionInputs(t *testing.T, root string) (specPath, policyPath, publicationPath string) {
	t.Helper()
	specPath = filepath.Join(root, "spec.md")
	policyPath = filepath.Join(root, "policy.json")
	publicationPath = filepath.Join(root, "publication.json")
	if err := os.WriteFile(specPath, []byte("# Work item\n\nImplement the thing.\n"), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	policy := submissionPolicyBody("daemon/**", strings.Repeat("ab", 32))
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	publication := `{"title":"Test the work item","body":"## Why\n\nCloses #123.\n","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`
	if err := os.WriteFile(publicationPath, []byte(publication), 0o600); err != nil {
		t.Fatalf("write publication metadata: %v", err)
	}
	return specPath, policyPath, publicationPath
}

func submissionPolicyBody(paths, digestHex string) string {
	provenance := `,"provenance":{"source":"override","digest":"sha256:` + digestHex + `"}}`
	return `[{"key":"paths","value":"` + paths + `"` + provenance +
		`,{"key":"gates.spec_approval","value":"true"` + provenance +
		`,{"key":"elaboration.max_iterations","value":"4"` + provenance +
		`,{"key":"budgets.stage_active_time","value":"1h"` + provenance +
		`,{"key":"waiting.spec_approval_attention_after","value":"1m"` + provenance +
		`,{"key":"research.allowlist","value":"example.com"` + provenance +
		`,{"key":"research.max_response_bytes","value":"1024"` + provenance + `]`
}

func TestSubmitCommandRegistersAndConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit",
	}

	first, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := loadOrCreateTopicKey(cfg.DBPath, true); err != nil {
		t.Fatalf("fresh submit did not initialize the daemon topic key: %v", err)
	}
	if !strings.HasPrefix(string(first.RunID), "run-") {
		t.Fatalf("default run id = %q, want run-<digest>", first.RunID)
	}
	if got := len(strings.TrimPrefix(string(first.RunID), "run-")); got != sha256.Size*2 {
		t.Fatalf("default run id digest length = %d, want %d", got, sha256.Size*2)
	}
	if first.ElaborationRunID == "" || first.ElaborationRunID == first.RunID ||
		first.ImplementationRunID != first.RunID ||
		first.InvocationID != first.ImplementationInvocationID ||
		first.StageID != first.ImplementationStageID ||
		!strings.HasPrefix(string(first.ImplementationInvocationID), "inv-implement-") ||
		!strings.HasPrefix(string(first.ElaborationInvocationID), "inv-elaborate-") ||
		first.ElaborationInvocationID == first.ImplementationInvocationID ||
		first.ElaborationStageID == first.ImplementationStageID {
		t.Fatalf("submission identities = %+v, want distinct elaboration and implementation runs", first)
	}
	if first.SpecDigest == "" || first.PolicyDigest == "" || first.SpecDigest == first.PolicyDigest {
		t.Fatalf("digests = %q/%q, want distinct content digests", first.SpecDigest, first.PolicyDigest)
	}
	if first.PublicationDigest == "" {
		t.Fatal("publication metadata has no durable digest binding")
	}

	replay, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("replay submit: %v", err)
	}
	if replay != first {
		t.Fatalf("replay result = %#v, want %#v", replay, first)
	}

	otherProject := cfg
	otherProject.ProjectID = "proj-other"
	otherProjectResult, err := runSubmitCommand(ctx, otherProject)
	if err != nil {
		t.Fatalf("same specification in another project: %v", err)
	}
	if otherProjectResult.RunID == first.RunID ||
		otherProjectResult.SpecDigest != first.SpecDigest {
		t.Fatalf("other-project result = %#v, want same spec under a distinct run", otherProjectResult)
	}

	otherPolicy := cfg
	otherPolicy.PolicyPath = filepath.Join(root, "other-policy.json")
	otherPolicyBody := submissionPolicyBody("app/**", strings.Repeat("cd", 32))
	if err := os.WriteFile(otherPolicy.PolicyPath, []byte(otherPolicyBody), 0o600); err != nil {
		t.Fatalf("write other policy: %v", err)
	}
	otherPolicyResult, err := runSubmitCommand(ctx, otherPolicy)
	if err != nil {
		t.Fatalf("same specification under another policy: %v", err)
	}
	if otherPolicyResult.RunID == first.RunID ||
		otherPolicyResult.PolicyDigest == first.PolicyDigest {
		t.Fatalf("other-policy result = %#v, want distinct policy and run", otherPolicyResult)
	}

	otherPublication := cfg
	otherPublication.PublicationPath = filepath.Join(root, "other-publication.json")
	if err := os.WriteFile(
		otherPublication.PublicationPath,
		[]byte(`{"title":"Describe another outcome","body":"## Why\n\nCloses #456.\n","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`),
		0o600,
	); err != nil {
		t.Fatalf("write other publication metadata: %v", err)
	}
	otherPublicationResult, err := runSubmitCommand(ctx, otherPublication)
	if err != nil {
		t.Fatalf("same work under other publication metadata: %v", err)
	}
	if otherPublicationResult.RunID == first.RunID ||
		otherPublicationResult.PublicationDigest == first.PublicationDigest {
		t.Fatalf("other-publication result = %#v, want distinct metadata and run", otherPublicationResult)
	}

	// The durable state a replayed submission converged on: the private
	// elaboration run, artifacts, invocation, and pending dispatch intent. The
	// implementation run remains only a reservation until approval.
	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(ctx, first.RunID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pre-approval implementation run, want absent: %w", err)
		}
		run, err := tx.GetRun(ctx, first.ElaborationRunID)
		if err != nil {
			return err
		}
		if run.SpecDigest != first.SpecDigest || run.PolicyDigest != first.PolicyDigest {
			t.Errorf("run digests = %q/%q, want %q/%q",
				run.SpecDigest, run.PolicyDigest, first.SpecDigest, first.PolicyDigest)
		}
		observation, err := tx.ObserveRun(ctx, first.ElaborationRunID)
		if err != nil {
			return err
		}
		if len(observation.Milestones) != 1 ||
			observation.Milestones[0].Kind != domain.MilestoneRunSubmitted ||
			observation.Milestones[0].InvocationID == nil ||
			*observation.Milestones[0].InvocationID != first.ElaborationInvocationID {
			t.Errorf("elaboration submission milestones = %+v, want one run_submitted milestone", observation.Milestones)
		}
		resolved, err := tx.GetResolvedPolicy(ctx, first.ElaborationRunID)
		if err != nil {
			return err
		}
		if resolved.Digest != first.PolicyDigest || len(resolved.Keys) != 7 {
			t.Errorf("resolved policy = %#v, want the run-bound submitted policy", resolved)
		}
		artifact, err := tx.GetArtifact(ctx, first.SpecArtifactID)
		if err != nil {
			return err
		}
		if artifact.Digest != first.SpecDigest || artifact.PublishEligible {
			t.Errorf("spec artifact = %#v, want submitted digest, never publish-eligible", artifact)
		}
		entry, err := tx.GetOutbox(ctx, string(first.ElaborationInvocationID))
		if err != nil {
			return err
		}
		if entry.Dispatched() {
			t.Error("dispatch intent already dispatched; submit must leave dispatch to the engine")
		}
		invocation, err := tx.GetAgentInvocation(ctx, first.ElaborationInvocationID)
		if err != nil {
			return err
		}
		if invocation.ConversationID != nil {
			t.Errorf("invocation binds conversation %v, want artifact-bound", *invocation.ConversationID)
		}
		return nil
	}); err != nil {
		t.Fatalf("read submitted state: %v", err)
	}
	var submittedPolicy domain.ResolvedPolicy
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		submittedPolicy, err = tx.GetResolvedPolicy(ctx, first.ElaborationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	authorityRunID := domain.RunID("authority-" + string(first.RunID))
	authorityPolicy, err := domain.NewResolvedPolicy(authorityRunID, submittedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	authoritySubmission, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: authorityRunID, ProjectID: first.ProjectID,
		SpecArtifactID: first.SpecArtifactID, PolicyArtifactID: first.PolicyArtifactID,
		ResolvedPolicy: authorityPolicy,
		Publication: engine.ProductionPublication{
			Title: "Test the work item", Body: "## Why\n\nCloses #123.\n",
			CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	})
	if err != nil {
		t.Fatalf("submit authority fixture: %v", err)
	}
	otherAuthorityRunID := domain.RunID("authority-" + string(otherPublicationResult.RunID))
	otherAuthorityPolicy, err := domain.NewResolvedPolicy(otherAuthorityRunID, submittedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	otherAuthoritySubmission, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: otherAuthorityRunID, ProjectID: otherPublicationResult.ProjectID,
		SpecArtifactID:   otherPublicationResult.SpecArtifactID,
		PolicyArtifactID: otherPublicationResult.PolicyArtifactID,
		ResolvedPolicy:   otherAuthorityPolicy,
		Publication: engine.ProductionPublication{
			Title: "Describe another outcome", Body: "## Why\n\nCloses #456.\n",
			CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	})
	if err != nil {
		t.Fatalf("submit second authority fixture: %v", err)
	}
	authority := storeAdmissionAuthority{
		store: st,
		commitAuthors: fixedProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
			AppSlug: "freeside-test", BotUserID: 12345,
		}},
	}
	commitAuthorErr := errors.New("commit author resolution must not run for elaboration")
	authority.commitAuthors = fixedProductionCommitAuthorResolver{err: commitAuthorErr}
	if err := authority.authenticateInvocationStart(ctx, first.ElaborationInvocationID,
		domain.ExecutionAdmission{
			RunID: first.ElaborationRunID, StageID: first.ElaborationStageID,
			OperatingMode: domain.ModeUnattended,
		}, "example/repo"); err != nil {
		t.Fatalf("authenticate elaboration start without publication author: %v", err)
	}
	if err := authority.authenticateInvocationStart(ctx, first.ElaborationInvocationID,
		domain.ExecutionAdmission{
			RunID: authorityRunID, StageID: first.ElaborationStageID,
			OperatingMode: domain.ModeUnattended,
		}, "example/repo"); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("authenticate elaboration start with foreign run = %v, want ErrParentKeyMismatch", err)
	}
	elaborationAdmission := domain.ExecutionAdmission{
		RunID: first.ElaborationRunID, StageID: first.ElaborationStageID,
		OperatingMode: domain.ModeUnattended,
	}
	if author, production, err := authority.invocationImportAuthor(
		ctx, first.ElaborationInvocationID, elaborationAdmission, "example/repo",
	); err != nil || production || author != (engine.ProductionCommitAuthor{}) {
		t.Fatalf("authenticate elaboration import author = %#v, production=%t, err=%v", author, production, err)
	}
	if author, production, err := authority.invocationImportRecordAuthor(
		ctx, first.ElaborationInvocationID, elaborationAdmission,
	); err != nil || production || author != (engine.ProductionCommitAuthor{}) {
		t.Fatalf("authenticate elaboration replay author = %#v, production=%t, err=%v", author, production, err)
	}
	// An elaboration invocation imports under the specification finding profile,
	// so investigation debris does not definitively fail it (#768). Both
	// ImportOptions and ImportOptionsRecord set it through this one helper, so
	// the live import and its terminal replay reconstruct the same profile.
	var elaborationOpts importer.Options
	if err := authority.applyElaborationFindingProfile(
		ctx, first.ElaborationInvocationID, elaborationAdmission, &elaborationOpts,
	); err != nil {
		t.Fatalf("apply elaboration finding profile: %v", err)
	}
	if elaborationOpts.Policy.FindingProfile == nil ||
		*elaborationOpts.Policy.FindingProfile != importer.FindingProfileSpecification {
		t.Fatalf("elaboration finding profile = %v, want specification",
			elaborationOpts.Policy.FindingProfile)
	}
	// A production invocation is left with a nil profile (the default
	// publish-strict, omitted on the wire), so its recorded ImportOptions and
	// publication task payload are byte-identical.
	var productionOpts importer.Options
	if err := authority.applyElaborationFindingProfile(
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
	authority.commitAuthors = fixedProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
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
	countingResolver := &countingProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
		AppSlug: "freeside-test", BotUserID: 12345,
	}}
	authority.commitAuthors = countingResolver
	authority.authenticatedStartAuthors = newProductionCommitAuthorAuthenticationCache()
	for range 2 {
		if _, _, err := authority.authenticateProductionCommitAuthorForStart(
			ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "example/repo",
		); err != nil {
			t.Fatalf("authenticate cached production start author: %v", err)
		}
	}
	if countingResolver.resolveCalls != 1 || countingResolver.revalidateCalls != 0 {
		t.Fatalf("stable start made %d resolve and %d revalidate calls, want 1 and 0",
			countingResolver.resolveCalls, countingResolver.revalidateCalls)
	}
	if _, _, err := authority.authenticateProductionCommitAuthorForStart(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "another/repo",
	); err != nil {
		t.Fatalf("re-authenticate changed start binding: %v", err)
	}
	authority.authenticatedStartAuthors.forget(authoritySubmission.InvocationID)
	if _, _, err := authority.authenticateProductionCommitAuthorForStart(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "another/repo",
	); err != nil {
		t.Fatalf("re-authenticate forgotten start binding: %v", err)
	}
	if countingResolver.resolveCalls != 3 {
		t.Fatalf("changed and forgotten starts made %d resolve calls, want 3", countingResolver.resolveCalls)
	}
	if _, _, err := authority.authenticateProductionCommitAuthorRevalidated(
		ctx, authoritySubmission.InvocationID, domain.ModeUnattended, "another/repo",
	); err != nil {
		t.Fatalf("revalidate production author before import: %v", err)
	}
	if countingResolver.revalidateCalls != 1 {
		t.Fatalf("import boundary made %d revalidate calls, want 1", countingResolver.revalidateCalls)
	}
	countingResolver.identity = publish.AppBotIdentity{AppSlug: "another-app", BotUserID: 67890}
	if _, _, err := authority.authenticateProductionCommitAuthorForStart(
		ctx, otherAuthoritySubmission.InvocationID, domain.ModeUnattended, "example/repo",
	); !errors.Is(err, publish.ErrAppBotIdentityMismatch) {
		t.Fatalf("new invocation under changed App binding = %v, want ErrAppBotIdentityMismatch", err)
	}
	authority.commitAuthors = fixedProductionCommitAuthorResolver{identity: publish.AppBotIdentity{
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
}

func TestSubmitCommandReplaysMatchingPreElaborationProductionRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit",
	}
	spec, err := readSubmissionFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	policyFile, err := readSubmissionFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	var keys []domain.PolicyKey
	if err := json.Unmarshal(policyFile.body, &keys); err != nil {
		t.Fatal(err)
	}
	policyDigest, err := (domain.ResolvedPolicy{Keys: keys}).ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	publication := engine.ProductionPublication{
		Title: "Test the work item", Body: "## Why\n\nCloses #123.\n",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	publicationDigest := submissionBytes(publicationBody).digest
	runID := defaultSubmissionRunID(cfg.ProjectID, spec.digest, policyDigest, publicationDigest, "")
	resolved, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		t.Fatal(err)
	}
	policyBody, err := json.Marshal(resolved.Keys)
	if err != nil {
		t.Fatal(err)
	}
	policy := submissionBytes(policyBody)
	specArtifact, err := submissionArtifact(domain.ArtifactKindSpecification, spec.digest)
	if err != nil {
		t.Fatal(err)
	}
	policyArtifact, err := submissionArtifact(domain.ArtifactKindPolicy, policy.digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateTopicKey(cfg.DBPath, false); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(spec.digest, strings.NewReader(string(spec.body))); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(policy.digest, strings.NewReader(string(policy.body))); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, specArtifact); err != nil {
			return err
		}
		return tx.PutArtifact(ctx, policyArtifact)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: runID, ProjectID: cfg.ProjectID, SpecArtifactID: specArtifact.ID,
		PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolved, Publication: publication,
	}); err != nil {
		t.Fatalf("seed pre-elaboration production run: %v", err)
	}

	replay, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("replay pre-elaboration production run: %v", err)
	}
	if replay.RunID != runID || replay.ImplementationRunID != runID ||
		replay.ElaborationRunID != "" || replay.ElaborationInvocationID != "" ||
		replay.ElaborationStageID != "" {
		t.Fatalf("legacy replay result = %+v", replay)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		elaborationRunID, err := engine.ElaborationRunIDForImplementation(runID)
		if err != nil {
			return err
		}
		_, err = tx.GetRun(ctx, elaborationRunID)
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("legacy replay created elaboration run: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	issue := 123
	workUnit := &domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &issue,
		DeclaredPaths:       declaredPathScope(keys),
	}
	declaration, err := domain.NewWorkUnitDeclaration(*workUnit, runID, cfg.ProjectID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
	legacy, found, err := legacyProductionReplay(ctx, st, runID, cfg.ProjectID,
		specArtifact, policyArtifact, keys, publication, workUnit, publicationDigest)
	if err != nil || !found {
		t.Fatalf("legacy work-unit replay = %#v, found=%t, err=%v", legacy, found, err)
	}
	if legacy.WorkUnitID != domain.WorkUnitIDForRun(runID) {
		t.Fatalf("legacy work-unit id = %q, want %q", legacy.WorkUnitID, domain.WorkUnitIDForRun(runID))
	}
}

func TestStoreAdmissionAuthorityDerivesFallbackCommitMessage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "freeside.db"), SpecPath: specPath,
		PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit-message",
	}
	submitted, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	var submittedPolicy domain.ResolvedPolicy
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		submittedPolicy, err = tx.GetResolvedPolicy(ctx, submitted.ElaborationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	productionRunID := domain.RunID("fallback-" + string(submitted.RunID))
	productionPolicy, err := domain.NewResolvedPolicy(productionRunID, submittedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	production, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: productionRunID, ProjectID: submitted.ProjectID,
		SpecArtifactID: submitted.SpecArtifactID, PolicyArtifactID: submitted.PolicyArtifactID,
		ResolvedPolicy: productionPolicy,
		Publication: engine.ProductionPublication{
			Title: "Test the work item", Body: "## Why\n\nCloses #123.\n",
			CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	authority := storeAdmissionAuthority{store: st, blobs: blobs}
	admission := domain.ExecutionAdmission{
		RunID: production.Run.ID, SpecDigest: submitted.SpecDigest,
	}
	got, err := authority.fallbackCommitMessage(ctx, admission, importer.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if subject, _, _ := strings.Cut(got, "\n"); subject != "Work item" {
		t.Fatalf("undeclared subject = %q, want %q", subject, "Work item")
	}

	issue := 123
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &issue,
		DeclaredPaths:       []string{"daemon/**"},
	}, production.Run.ID, domain.ProjectID(cfg.ProjectID), time.Now().UTC())
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
	if subject, _, _ := strings.Cut(got, "\n"); subject != "Work item (#123)" {
		t.Fatalf("issue-bound subject = %q, want %q", subject, "Work item (#123)")
	}
	reconstructed, err := authority.fallbackCommitMessage(ctx, admission, importer.Policy{})
	if err != nil || reconstructed != got {
		t.Fatalf("reconstructed message = %q, %v; want byte-identical %q", reconstructed, err, got)
	}
}

func TestSubmitRefusesAnExistingStoreWithoutItsTopicKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	dbPath := filepath.Join(root, "freeside.db")
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("create existing store: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close existing store: %v", err)
	}

	_, err = runSubmitCommand(ctx, submitCommandConfig{
		DBPath: dbPath, SpecPath: specPath, PolicyPath: policyPath,
		PublicationPath: publicationPath,
		ProjectID:       "proj-submit",
	})
	if !errors.Is(err, errTopicKeyMissing) {
		t.Fatalf("submit error = %v, want errTopicKeyMissing", err)
	}
}

func TestSubmitCommandRefusesBadInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	base := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "proj-submit",
	}

	missing := base
	missing.SpecPath = filepath.Join(root, "absent.md")
	if _, err := runSubmitCommand(ctx, missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing spec error = %v, want ErrNotExist", err)
	}

	empty := base
	empty.SpecPath = filepath.Join(root, "empty.md")
	if err := os.WriteFile(empty.SpecPath, nil, 0o600); err != nil {
		t.Fatalf("write empty spec: %v", err)
	}
	if _, err := runSubmitCommand(ctx, empty); err == nil {
		t.Fatal("empty spec was accepted")
	}

	invalidPolicy := base
	invalidPolicy.PolicyPath = filepath.Join(root, "invalid-policy.json")
	if err := os.WriteFile(invalidPolicy.PolicyPath, []byte(`{"paths":["daemon/**"]}`), 0o600); err != nil {
		t.Fatalf("write invalid policy: %v", err)
	}
	if _, err := runSubmitCommand(ctx, invalidPolicy); err == nil {
		t.Fatal("an opaque policy document was accepted as a resolved policy")
	}
	duplicatePolicy := base
	duplicatePolicy.PolicyPath = filepath.Join(root, "duplicate-policy.json")
	duplicateBody := `[{"key":"paths","key":"egress","value":"daemon/**","provenance":{"source":"override","digest":"sha256:` +
		strings.Repeat("ab", 32) + `"}}]`
	if err := os.WriteFile(duplicatePolicy.PolicyPath, []byte(duplicateBody), 0o600); err != nil {
		t.Fatalf("write duplicate-key policy: %v", err)
	}
	if _, err := runSubmitCommand(ctx, duplicatePolicy); err == nil {
		t.Fatal("a policy with duplicate JSON keys was accepted")
	}

	for name, body := range map[string]string{
		"missing title":      `{"body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`,
		"multiline title":    `{"title":"Bad\ntitle","body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`,
		"unknown field":      `{"title":"Test work","body":"Reviewer context.","summary":"hidden","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`,
		"duplicate field":    `{"title":"First","title":"Second","body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`,
		"publication marker": `{"title":"Test work","body":"<!-- freeside:publication-identity=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -->","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`,
		"missing body":       `{"title":"Test work","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`,
		"missing author":     `{"title":"Test work","body":"Reviewer context."}`,
		"invalid app slug":   `{"title":"Test work","body":"Reviewer context.","commit_author":{"app_slug":"Freeside Test","bot_user_id":12345}}`,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := base
			invalid.PublicationPath = filepath.Join(root, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(invalid.PublicationPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := runSubmitCommand(ctx, invalid); err == nil {
				t.Fatalf("publication metadata %q was accepted", body)
			}
		})
	}

	noProject := base
	noProject.ProjectID = ""
	if _, err := runSubmitCommand(ctx, noProject); err == nil {
		t.Fatal("missing project was accepted")
	}

	// Same run id, different content: the run's fixed bindings refuse the
	// retarget instead of silently replacing the approved specification.
	pinned := base
	pinned.RunID = "run-pinned"
	if _, err := runSubmitCommand(ctx, pinned); err != nil {
		t.Fatalf("pinned submit: %v", err)
	}
	changed := pinned
	changed.SpecPath = filepath.Join(root, "changed.md")
	if err := os.WriteFile(changed.SpecPath, []byte("# Different work item\n"), 0o600); err != nil {
		t.Fatalf("write changed spec: %v", err)
	}
	if _, err := runSubmitCommand(ctx, changed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("retargeting submit error = %v, want ErrImmutableTransition", err)
	}
	changedPublication := pinned
	changedPublication.PublicationPath = filepath.Join(root, "changed-publication.json")
	if err := os.WriteFile(
		changedPublication.PublicationPath,
		[]byte(`{"title":"Change the public outcome","body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runSubmitCommand(ctx, changedPublication); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("publication retargeting error = %v, want ErrImmutableTransition", err)
	}
}

func TestSubmissionArtifactIdentityRetainsFullDigest(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("ab", 6)
	firstDigest := domain.Digest("sha256:" + prefix + strings.Repeat("1", 52))
	secondDigest := domain.Digest("sha256:" + prefix + strings.Repeat("2", 52))
	first, err := submissionArtifact(domain.ArtifactKindSpecification, firstDigest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := submissionArtifact(domain.ArtifactKindSpecification, secondDigest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("same-prefix digests reused artifact ID %q", first.ID)
	}
	if first.Provenance.ProducerInvocationID == second.Provenance.ProducerInvocationID {
		t.Fatalf("same-prefix digests reused producer ID %q",
			first.Provenance.ProducerInvocationID)
	}
	for _, artifact := range []domain.Artifact{first, second} {
		hexDigest := strings.TrimPrefix(string(artifact.Digest), "sha256:")
		if !strings.HasSuffix(string(artifact.ID), hexDigest) ||
			!strings.HasSuffix(string(artifact.Provenance.ProducerInvocationID), hexDigest) {
			t.Fatalf("artifact identity %#v does not retain its full digest", artifact)
		}
	}
}

// TestSubmitCommandCapturesWorkUnitDeclaration (§5.18, issue #443): a
// --work-unit submission reserves the declaration with its path scope
// derived from the resolved policy's paths key, joins the declaration into
// the default run-id derivation (a different declaration is a different
// run; an undeclared submission keeps the pre-capture derivation), and
// refuses a declaration file with unknown fields.
func TestSubmitCommandCapturesWorkUnitDeclaration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	workUnitPath := filepath.Join(root, "work-unit.json")
	declaration := `{"completion_criterion":"bound_issue_closed_by_merged_pr","bound_issue":443,"depends_on_issues":[442,440,442],"contract_serialized":true}`
	if err := os.WriteFile(workUnitPath, []byte(declaration), 0o600); err != nil {
		t.Fatalf("write work-unit declaration: %v", err)
	}
	cfg := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		WorkUnitPath: workUnitPath,
		ProjectID:    "proj-submit",
	}

	declared, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if declared.WorkUnitID != domain.WorkUnitIDForRun(declared.RunID) {
		t.Fatalf("work unit id = %q, want derived from run %q", declared.WorkUnitID, declared.RunID)
	}

	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	var stored domain.WorkUnitDeclarationInput
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetWorkUnitDeclarationByRun(ctx, declared.RunID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pre-approval implementation declaration, want absent: %w", err)
		}
		entry, err := tx.GetOutbox(ctx, string(declared.ElaborationInvocationID))
		if err != nil {
			return err
		}
		var payload struct {
			WorkUnit *domain.WorkUnitDeclarationInput `json:"work_unit"`
		}
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			return err
		}
		if payload.WorkUnit == nil {
			return errors.New("elaboration reservation omitted work-unit declaration")
		}
		stored = *payload.WorkUnit
		return nil
	}); err != nil {
		t.Fatalf("read reserved declaration: %v", err)
	}
	if stored.BoundIssue == nil || *stored.BoundIssue != 443 || !stored.ContractSerialized {
		t.Fatalf("stored declaration = %+v", stored)
	}
	// The unsorted, duplicated dependency list canonicalized at intake.
	if len(stored.DependsOnIssues) != 2 || stored.DependsOnIssues[0] != 440 || stored.DependsOnIssues[1] != 442 {
		t.Fatalf("stored dependencies = %v, want canonical [440 442]", stored.DependsOnIssues)
	}
	// The declared path scope is the policy's paths key, never a second
	// operator statement that could drift from what the runner enforces.
	if len(stored.DeclaredPaths) != 1 || stored.DeclaredPaths[0] != "daemon/**" {
		t.Fatalf("stored declared paths = %v, want the policy allowlist", stored.DeclaredPaths)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// The declaration joins the default run-id derivation: the same inputs
	// without it converge on a different (pre-capture) run id.
	undeclared := cfg
	undeclared.WorkUnitPath = ""
	undeclared.DBPath = filepath.Join(root, "undeclared.db")
	plain, err := runSubmitCommand(ctx, undeclared)
	if err != nil {
		t.Fatalf("undeclared submit: %v", err)
	}
	if plain.RunID == declared.RunID {
		t.Fatal("declared and undeclared submissions converged on one run id")
	}
	if plain.WorkUnitID != "" {
		t.Fatalf("undeclared submission reports work unit %q", plain.WorkUnitID)
	}

	// Unknown declaration fields are refused, not silently dropped.
	badPath := filepath.Join(root, "bad-work-unit.json")
	if err := os.WriteFile(badPath, []byte(`{"completion_criterion":"bound_pr_merged","declared_paths":["daemon/"]}`), 0o600); err != nil {
		t.Fatalf("write bad declaration: %v", err)
	}
	bad := cfg
	bad.WorkUnitPath = badPath
	bad.DBPath = filepath.Join(root, "bad.db")
	if _, err := runSubmitCommand(ctx, bad); err == nil ||
		!strings.Contains(err.Error(), "decode work-unit declaration") {
		t.Fatalf("unknown-field declaration error = %v", err)
	}
}

// TestSubmitCommandEmptyDependencyDeclarationConverges: an explicit empty
// depends_on_issues list survives the store's omitempty round-trip (empty
// collections normalize to nil in the constructor), so an exact
// re-submission converges instead of refusing its own stored declaration.
func TestSubmitCommandEmptyDependencyDeclarationConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	workUnitPath := filepath.Join(root, "work-unit.json")
	declaration := `{"completion_criterion":"bound_pr_merged","depends_on_issues":[]}`
	if err := os.WriteFile(workUnitPath, []byte(declaration), 0o600); err != nil {
		t.Fatalf("write work-unit declaration: %v", err)
	}
	cfg := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath, PublicationPath: publicationPath,
		WorkUnitPath: workUnitPath,
		ProjectID:    "proj-submit",
	}
	first, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	replay, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("empty-dependency replay must converge: %v", err)
	}
	if replay.RunID != first.RunID || replay.WorkUnitID != first.WorkUnitID {
		t.Fatalf("replay identities differ: %+v vs %+v", replay, first)
	}
}
