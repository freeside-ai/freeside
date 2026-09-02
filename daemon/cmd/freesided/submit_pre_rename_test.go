package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestSubmitCommandReplaysPreRenameDatabase re-submits the exact work item
// a pre-rename daemon accepted (the store package's frozen dump) and expects
// the replay to converge on the legacy specification run instead of minting
// a second one for the same implementation identity.
func TestSubmitCommandReplaysPreRenameDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dump, err := os.ReadFile(filepath.Join(
		"..", "..", "internal", "store", "testdata", "pre_rename_specification_vocabulary.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "state.db")
	// The submit command refuses an existing store without its topic key;
	// mint one the way a first submission would have.
	if _, err := loadOrCreateTopicKey(dbPath, false); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(context.Background(), string(dump)); err != nil {
		t.Fatalf("load pre-rename dump: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// The same submission inputs the capture used, byte for byte.
	workItemPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	manifestBody, err := json.Marshal([]domain.CapabilityManifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	var policyKeys []domain.PolicyKey
	if err := json.Unmarshal(
		[]byte(submissionPolicyBody("daemon/**", strings.Repeat("ab", 32))), &policyKeys,
	); err != nil {
		t.Fatal(err)
	}
	// The captured run was resolved under the pre-rename policy key; the
	// replay must present the same keys or the policy digest, and so the run,
	// legitimately differs.
	for i := range policyKeys {
		if suffix, ok := strings.CutPrefix(policyKeys[i].Key, "specification."); ok {
			policyKeys[i].Key = "elaboration." + suffix
		}
	}
	policyKeys = append(policyKeys, domain.PolicyKey{
		Key: domain.CapabilityManifestPolicyKey, Value: string(manifestBody),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: "sha256:capability-policy",
		},
	})
	policyBody, err := json.Marshal(policyKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, policyBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workItemPath, []byte("# Work item\n\nImplement the thing."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := submitCommandConfig{
		DBPath: dbPath, WorkItemPath: workItemPath,
		PolicyPath: policyPath, PublicationPath: publicationPath,
		ProjectID: "project-submit-elaboration", RunID: "implementation-from-submit",
	}
	replayed, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatalf("replay submission against the pre-rename database: %v", err)
	}
	const legacyRun = "run-elaboration-694193339c5545bbeabec19b6fc46182625db76693811ba435eb18dcb2601160"
	if replayed.SpecificationRunID != legacyRun || replayed.RunID != cfg.RunID ||
		replayed.SpecificationInvocationID != "inv-elaborate-"+legacyRun+"-1" ||
		replayed.SpecificationStageID != "elaborate-"+legacyRun {
		t.Fatalf("replayed submission = %+v, want the legacy specification identities", replayed)
	}
}
