package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func writeSubmissionInputs(t *testing.T, root string) (specPath, policyPath string) {
	t.Helper()
	specPath = filepath.Join(root, "spec.md")
	policyPath = filepath.Join(root, "policy.json")
	if err := os.WriteFile(specPath, []byte("# Work item\n\nImplement the thing.\n"), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	policy := `[{"key":"paths","value":"daemon/**","provenance":{"source":"override","digest":"sha256:` +
		strings.Repeat("ab", 32) + `"}}]`
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return specPath, policyPath
}

func TestSubmitCommandRegistersAndConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath,
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
	if first.SpecDigest == "" || first.PolicyDigest == "" || first.SpecDigest == first.PolicyDigest {
		t.Fatalf("digests = %q/%q, want distinct content digests", first.SpecDigest, first.PolicyDigest)
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
	otherPolicyBody := `[{"key":"paths","value":"app/**","provenance":{"source":"override","digest":"sha256:` +
		strings.Repeat("cd", 32) + `"}}]`
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

	// The durable state a replayed submission converged on: run, artifacts,
	// invocation, and pending dispatch intent.
	st, err := store.Open(ctx, cfg.DBPath, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, first.RunID)
		if err != nil {
			return err
		}
		if run.SpecDigest != first.SpecDigest || run.PolicyDigest != first.PolicyDigest {
			t.Errorf("run digests = %q/%q, want %q/%q",
				run.SpecDigest, run.PolicyDigest, first.SpecDigest, first.PolicyDigest)
		}
		resolved, err := tx.GetResolvedPolicy(ctx, first.RunID)
		if err != nil {
			return err
		}
		if resolved.Digest != first.PolicyDigest || len(resolved.Keys) != 1 {
			t.Errorf("resolved policy = %#v, want the run-bound submitted policy", resolved)
		}
		artifact, err := tx.GetArtifact(ctx, first.SpecArtifactID)
		if err != nil {
			return err
		}
		if artifact.Digest != first.SpecDigest || artifact.PublishEligible {
			t.Errorf("spec artifact = %#v, want submitted digest, never publish-eligible", artifact)
		}
		entry, err := tx.GetOutbox(ctx, string(first.InvocationID))
		if err != nil {
			return err
		}
		if entry.Dispatched() {
			t.Error("dispatch intent already dispatched; submit must leave dispatch to the engine")
		}
		invocation, err := tx.GetAgentInvocation(ctx, first.InvocationID)
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
}

func TestSubmitRefusesAnExistingStoreWithoutItsTopicKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath := writeSubmissionInputs(t, root)
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
		ProjectID: "proj-submit",
	})
	if !errors.Is(err, errTopicKeyMissing) {
		t.Fatalf("submit error = %v, want errTopicKeyMissing", err)
	}
}

func TestSubmitCommandRefusesBadInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	specPath, policyPath := writeSubmissionInputs(t, root)
	base := submitCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		SpecPath: specPath, PolicyPath: policyPath,
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
}

func TestSubmissionArtifactIdentityRetainsFullDigest(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("ab", 6)
	firstDigest := domain.Digest("sha256:" + prefix + strings.Repeat("1", 52))
	secondDigest := domain.Digest("sha256:" + prefix + strings.Repeat("2", 52))
	first, err := submissionArtifact("specification", firstDigest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := submissionArtifact("specification", secondDigest)
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
