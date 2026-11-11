package integration_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestProductionVerificationLeavesAgentClaimsOut publishes a run whose agent
// recorded a claim of a check Freeside never ran. Since #3022 the Verification
// section lists only what Freeside ran; the claim is neither quoted nor listed,
// and the closing line labels the rest of the body as its author's account.
func TestProductionVerificationLeavesAgentClaimsOut(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	claimText := []byte("Passed: imaginary randomized fuzzing that Freeside never ran.\n")
	digest := productionDigest(claimText)
	putProductionBlob(t, p.publicationHarness, digest, claimText)
	p.replay.Evidence = export.EvidenceManifest{
		Version: export.EvidenceManifestVersion,
		Entries: []export.EvidenceEntry{{
			Label: "freeside.summary", MediaType: "text/markdown", Size: int64(len(claimText)), Digest: export.Digest(digest),
			Provenance: export.EvidenceProvenance{
				ProducerClass: export.EvidenceProducerAgent, ProducerInvocationID: string(p.replay.InvocationID),
				HeadBinding: export.EvidenceHeadBound, SourceHeadSHA: p.replay.HeadSHA,
				SensitivityClass: export.EvidenceSensitivityNormal,
			},
		}},
	}
	manifest, err := p.replay.Evidence.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := productionDigest(manifest)
	putProductionBlob(t, p.publicationHarness, manifestDigest, manifest)
	p.replay.EvidenceManifestDigest = &manifestDigest
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
	body := p.forge.PullRequests()[0].Body
	for _, absent := range []string{"imaginary randomized fuzzing", "freeside.summary", string(digest), "Agent-Reported"} {
		if strings.Contains(body, absent) {
			t.Fatalf("published body carries the agent claim (%q):\n%s", absent, body)
		}
	}
	if !strings.Contains(body, "- Passed: <code>/usr/bin/true</code>, run by Freeside at") ||
		!strings.Contains(body, "Freeside ran only the checks listed here.") {
		t.Fatalf("Verification section lost the executed command or its closing line:\n%s", body)
	}
}

func TestProductionVerificationMissingReportRetriesWithoutPublishing(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	var removedPath string
	var reportBytes []byte
	p.workflow = p.newEngine(t, productionCrashSeams{afterVerification: func() error {
		var checkpoint struct {
			Artifacts []domain.Artifact `json:"artifacts"`
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			entry, err := tx.GetInbox(p.ctx, "production-verification/"+string(p.runID)+"/"+p.replay.HeadSHA)
			if err != nil {
				return err
			}
			return json.Unmarshal(entry.Payload, &checkpoint)
		}); err != nil {
			return err
		}
		for _, artifact := range checkpoint.Artifacts {
			if artifact.Type == domain.ArtifactKindVerificationReport {
				removedPath = filepath.Join(p.blobDir, "sha256-"+strings.TrimPrefix(string(artifact.Digest), "sha256:"))
				reader, err := p.blobs.Open(artifact.Digest)
				if err != nil {
					return err
				}
				reportBytes, err = io.ReadAll(reader)
				if err := errors.Join(err, reader.Close()); err != nil {
					return err
				}
				return os.Remove(removedPath)
			}
		}
		t.Fatal("checkpoint lacks verifier report")
		return nil
	}}, true)
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || removedPath == "" {
		t.Fatalf("missing report result = %+v, %v", result, err)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatal("missing report reached publication")
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		hold, found, err := tx.GetRunHold(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if !found || hold.Reason != domain.HoldPublicationEnvironment {
			t.Fatalf("missing report hold = %+v, found %t", hold, found)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(removedPath, reportBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.now = p.now.Add(time.Minute)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
}

func TestSubmitProductionRunRefusesOperatorVerificationSection(t *testing.T) {
	t.Parallel()
	f := openProductionFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-owned-verification")
	publication := productionPublicationMetadata()
	publication.Body = "## Why\nA change.\n\n## Verification\nPassed: checks that have not run."
	_, err := engine.SubmitProductionRun(t.Context(), f.store, engine.ProductionRunSpec{
		RunID: "run-owned-verification", ProjectID: "proj-prod", SpecArtifactID: spec.ID,
		PolicyArtifactID: policy.ID, ResolvedPolicy: resolved, Publication: publication,
	})
	if err == nil || !strings.Contains(err.Error(), "verification section") {
		t.Fatalf("submit accepted prospective verification: %v", err)
	}
}
