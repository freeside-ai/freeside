package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func setSubmitWorkflowPolicy(
	t *testing.T, dbPath string, scope domain.WorkflowPolicyScope, project *domain.ProjectID,
	key domain.WorkflowPolicyKey, value *string,
) {
	t.Helper()
	st := storetest.Open(t, dbPath, store.Options{})
	defer func() { _ = st.Close() }()
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		_, err := engine.SetWorkflowPolicyTx(t.Context(), tx, engine.SetWorkflowPolicyRequest{
			Scope: scope, ProjectID: project, Key: key, Value: value, SetBy: "device:device-1",
		}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func readStoredSubmitPolicy(t *testing.T, dbPath string, runID domain.RunID) domain.ResolvedPolicy {
	t.Helper()
	st := storetest.Open(t, dbPath, store.Options{})
	defer func() { _ = st.Close() }()
	var policy domain.ResolvedPolicy
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		policy, err = tx.GetResolvedPolicy(t.Context(), runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return policy
}

func policyKeyMap(keys []domain.PolicyKey) map[string]domain.PolicyKey {
	byKey := make(map[string]domain.PolicyKey, len(keys))
	for _, k := range keys {
		byKey[k.Key] = k
	}
	return byKey
}

// TestSubmitResolvesWorkflowPolicyLayersAndReplays covers layer resolution on
// freesided submit (project over general over default) and replay stability
// after a later layer change, for both repeated --submission-id and
// --retry-submission-id.
func TestSubmitResolvesWorkflowPolicyLayersAndReplays(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	project := domain.ProjectID("proj-submit")
	cfg := submitCommandConfig{
		SubmissionID:    "layers-replay",
		DBPath:          filepath.Join(root, "freeside.db"),
		TaskPath:        taskPath,
		PolicyPath:      policyPath,
		WorkUnitPath:    declaredWorkUnit(t),
		PublicationPath: publicationPath,
		ProjectID:       project,
	}
	seedTemplateStore(t, cfg.DBPath)

	setSubmitWorkflowPolicy(t, cfg.DBPath, domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowGateIssueComment, new("true"))
	setSubmitWorkflowPolicy(t, cfg.DBPath, domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowFollowUpMaxPerDay, new("4"))
	setSubmitWorkflowPolicy(t, cfg.DBPath, domain.WorkflowPolicyScopeProject, &project, domain.WorkflowGateSourceIssueClosure, new("true"))
	setSubmitWorkflowPolicy(t, cfg.DBPath, domain.WorkflowPolicyScopeProject, &project, domain.WorkflowFollowUpMaxPerDay, new("2"))

	first, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	stored := readStoredSubmitPolicy(t, cfg.DBPath, first.SpecificationRunID)
	if stored.Digest != first.SpecificationPolicyDigest {
		t.Fatalf("stored policy digest = %s, result = %s", stored.Digest, first.SpecificationPolicyDigest)
	}
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := blobs.Verify(first.SpecificationPolicyDigest); err != nil || !ok {
		t.Fatalf("stored policy blob for %s: verified=%t, err=%v", first.SpecificationPolicyDigest, ok, err)
	}
	byKey := policyKeyMap(stored.Keys)
	wants := []struct {
		key        domain.WorkflowPolicyKey
		value      string
		provenance domain.ProvenanceSource
	}{
		{domain.WorkflowGateSourceIssueClosure, "true", domain.ProvenanceProject},
		{domain.WorkflowGateIssueComment, "true", domain.ProvenanceGeneral},
		{domain.WorkflowGateFollowUpFiling, "false", domain.ProvenancePreset},
		{domain.WorkflowFollowUpMaxPerDay, "2", domain.ProvenanceProject},
		{domain.WorkflowFollowUpMaxPerRun, "5", domain.ProvenancePreset},
		{domain.WorkflowFollowUpMaxDepth, "2", domain.ProvenancePreset},
	}
	for _, want := range wants {
		got, ok := byKey[string(want.key)]
		if !ok || got.Value != want.value || got.Provenance.Source != want.provenance {
			t.Fatalf("key %s = %+v (present=%t), want value=%q provenance=%q",
				want.key, got, ok, want.value, want.provenance)
		}
	}

	// Change a layer after submission; both repeat forms must return the
	// original result and keep the stored policy unchanged.
	setSubmitWorkflowPolicy(t, cfg.DBPath, domain.WorkflowPolicyScopeProject, &project, domain.WorkflowGateSourceIssueClosure, new("false"))

	sameID, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("repeat same submission-id: %v", err)
	}
	if sameID != first {
		t.Fatalf("repeat same submission-id = %+v, want %+v", sameID, first)
	}
	retried, err := runSubmitCommand(ctx, submitCommandConfig{
		DBPath: cfg.DBPath, RetrySubmissionID: cfg.SubmissionID,
	})
	if err != nil {
		t.Fatalf("retry-submission-id: %v", err)
	}
	if retried != first {
		t.Fatalf("retry-submission-id = %+v, want %+v", retried, first)
	}
	after := readStoredSubmitPolicy(t, cfg.DBPath, first.SpecificationRunID)
	if after.Digest != stored.Digest {
		t.Fatalf("stored policy digest changed after replay: got %s, want %s", after.Digest, stored.Digest)
	}
	if got := policyKeyMap(after.Keys)[string(domain.WorkflowGateSourceIssueClosure)]; got.Value != "true" || got.Provenance.Source != domain.ProvenanceProject {
		t.Fatalf("closure gate after replay = %+v, want unchanged project true", got)
	}
}

// TestSubmitFileWorkflowKeyDoesNotOverrideStoreAndWarns checks that a -policy
// file setting a pane-owned key loses to the store layers (defaulting to
// preset when unset) and produces a warning naming the key, value, and
// Workflow pane.
func TestSubmitFileWorkflowKeyDoesNotOverrideStoreAndWarns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)

	rawPolicy, err := os.ReadFile(policyPath) //nolint:gosec // fixture under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var fileKeys []domain.PolicyKey
	if err := json.Unmarshal(rawPolicy, &fileKeys); err != nil {
		t.Fatal(err)
	}
	if warnings := workflowPolicyFileWarnings(fileKeys); len(warnings) != 0 {
		t.Fatalf("warnings for file without pane keys = %v, want none", warnings)
	}
	fileKeys = append(fileKeys, domain.PolicyKey{
		Key:   string(domain.WorkflowGateSourceIssueClosure),
		Value: "true",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:" + domain.Digest(strings.Repeat("ab", 32)),
		},
	})
	warnings := workflowPolicyFileWarnings(fileKeys)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want 1 warning", warnings)
	}
	for _, want := range []string{string(domain.WorkflowGateSourceIssueClosure), `"true"`, "new work", "Workflow pane"} {
		if !strings.Contains(warnings[0], want) {
			t.Fatalf("warning %q missing %q", warnings[0], want)
		}
	}

	updatedPolicy, err := json.Marshal(fileKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, updatedPolicy, 0o600); err != nil {
		t.Fatal(err)
	}
	filePolicyDigest, err := (domain.ResolvedPolicy{Keys: fileKeys}).ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	cfg := submitCommandConfig{
		SubmissionID:    "file-override-ignored",
		DBPath:          filepath.Join(root, "freeside.db"),
		TaskPath:        taskPath,
		PolicyPath:      policyPath,
		WorkUnitPath:    declaredWorkUnit(t),
		PublicationPath: publicationPath,
		ProjectID:       "proj-submit",
	}
	seedTemplateStore(t, cfg.DBPath)
	result, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.SpecificationPolicyDigest == filePolicyDigest {
		t.Fatal("stored policy digest matched the file's digest despite layer overlay")
	}
	stored := readStoredSubmitPolicy(t, cfg.DBPath, result.SpecificationRunID)
	got := policyKeyMap(stored.Keys)[string(domain.WorkflowGateSourceIssueClosure)]
	if got.Value != "false" || got.Provenance.Source != domain.ProvenancePreset {
		t.Fatalf("stored closure gate = %+v, want false from preset", got)
	}
}

// TestSubmitReplaysHistoricalPolicyWithFileWorkflowKey checks that a
// submission stored before workflow policy layers applied to submit still
// replays with its original file-set workflow key and override provenance.
func TestSubmitReplaysHistoricalPolicyWithFileWorkflowKey(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	project := domain.ProjectID("proj-historical")
	spec := submissionBytes([]byte("# Task\n\nHistorical submission.\n"))
	provenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride,
		Digest: "sha256:" + domain.Digest(strings.Repeat("ab", 32)),
	}
	var keys []domain.PolicyKey
	if err := json.Unmarshal([]byte(submissionPolicyBody("daemon/**", strings.Repeat("ab", 32))), &keys); err != nil {
		t.Fatal(err)
	}
	historicalClosure := domain.PolicyKey{
		Key:        string(domain.WorkflowGateSourceIssueClosure),
		Value:      "true",
		Provenance: provenance,
	}
	keys = append(keys, historicalClosure)
	publication := engine.ProductionPublication{
		Title:        "Historical task",
		Body:         "Closes #99.",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
	publicationBody, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	publicationDigest := submissionBytes(publicationBody).digest
	policyDigest, err := (domain.ResolvedPolicy{Keys: keys}).ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	implRunID := engine.ManualSubmissionRunID("cli:historical-workflow", project, spec.digest, policyDigest, publicationDigest, "")
	specRunID, err := engine.SpecificationRunIDForImplementation(implRunID)
	if err != nil {
		t.Fatal(err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(implRunID)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPolicy, err := domain.NewResolvedPolicy(specRunID, keys)
	if err != nil {
		t.Fatal(err)
	}
	policyBody, err := json.Marshal(resolvedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	req := submitApplyRequest{
		SubmissionID:          "historical-workflow",
		ProjectID:             project,
		SpecBody:              spec.body,
		SpecDigest:            spec.digest,
		PolicyBody:            policyBody,
		PolicyDigest:          resolvedPolicy.Digest,
		Publication:           publication,
		PublicationDigest:     publicationDigest,
		PublicationBodyDigest: publicationDigest,
		Keys:                  keys,
		ResolvedPolicy:        resolvedPolicy,
		ImplementationRunID:   implRunID,
		SpecificationRunID:    specRunID,
		CampaignID:            campaignID,
	}

	dbPath := filepath.Join(root, "state.db")
	st := storetest.Open(t, dbPath, store.Options{})
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	seedHistoricalManualSubmission(t, st, blobs, req)

	// Set conflicting layers after the historical submission was stored.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := engine.SetWorkflowPolicyTx(ctx, tx, engine.SetWorkflowPolicyRequest{
			Scope: domain.WorkflowPolicyScopeProject, ProjectID: &project,
			Key: domain.WorkflowGateSourceIssueClosure, Value: new("false"), SetBy: "device:device-1",
		}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
			return err
		}
		_, err := engine.SetWorkflowPolicyTx(ctx, tx, engine.SetWorkflowPolicyRequest{
			Scope: domain.WorkflowPolicyScopeGeneral,
			Key:   domain.WorkflowGateIssueComment, Value: new("true"), SetBy: "device:device-1",
		}, time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	replayed, err := applySubmission(ctx, st, blobs, req)
	if err != nil {
		t.Fatalf("historical replay: %v", err)
	}
	if replayed.SpecificationPolicyDigest != req.PolicyDigest {
		t.Fatalf("replayed policy digest = %s, want historical %s", replayed.SpecificationPolicyDigest, req.PolicyDigest)
	}
	var stored domain.ResolvedPolicy
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetResolvedPolicy(ctx, specRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := policyKeyMap(stored.Keys)[string(domain.WorkflowGateSourceIssueClosure)]; got != historicalClosure {
		t.Fatalf("historical closure gate = %+v, want %+v", got, historicalClosure)
	}
}
