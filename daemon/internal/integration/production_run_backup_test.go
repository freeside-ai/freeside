package integration_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// A valid encrypted checkpoint can still fail final admission if the verifier
// cannot reconstruct a workflow kind that the daemon already wrote.
func TestRealRunBackupVerificationWorkflowClosure(t *testing.T) {
	t.Parallel()
	const input = "remediation input"
	digest := domain.Digest(contentaddr.Sum([]byte(input)))
	feedback := domain.OperatorFeedbackInvocationIntent{
		Version:      domain.OperatorFeedbackInvocationIntentVersion,
		InvocationID: "inv-operator-feedback-command", RunID: "run",
		StageID: "operator-feedback-inv-operator-feedback-command", CommandID: "command",
		ItemID: "ready", SourceInvocationID: "implementation",
		InputArtifactID: "operator-feedback-command", InputArtifactDigest: digest,
	}
	successor := domain.PublicationSuccessor{
		Version: domain.PublicationSuccessorVersion, RunID: feedback.RunID,
		CommandID: feedback.CommandID, FeedbackInvocationID: feedback.InvocationID,
		PredecessorItemID: feedback.ItemID, PriorReviewInvocationID: "review", ReviewRound: 2,
	}
	remediation := domain.RemediationInvocationIntent{
		Version: "freeside.remediation-request/v1", RunID: "run", Round: 1,
		InvocationID: "inv-remediate-1-run", StageID: "remediate-1-run",
		ReviewInvocationID: engine.ProductionReviewInvocationID("run", 1),
		AdjudicationDigest: digest, InputArtifactID: "remediation-input-1-run",
		InputArtifactDigest: digest, BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		FindingIDs: []domain.FindingID{"finding"},
	}
	continuation := domain.PublicationContinuationIntent{
		Version: domain.PublicationContinuationIntentVersion,
		Successor: domain.PublicationSuccessor{
			Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorRemediation,
			RunID: "run", CommandID: "continue", ReevaluationCommandID: "recheck",
			PredecessorItemID: "ready", PriorReviewInvocationID: "review", ReviewRound: 2,
		},
		SourceTaskKey: "production-publication/run", SourceTaskDigest: digest, AdjudicationDigest: digest,
	}
	discussion := domain.SpecificationDiscussionInvocationIntent{
		Version:            domain.SpecificationDiscussionInvocationIntentVersion,
		SpecificationRunID: "specification-run", ImplementationRunID: "run", ProjectID: "project", Iteration: 1,
		InvocationID: domain.SpecificationDiscussionInvocationID("command"), DiscussInvocationID: "inv-command",
		ConversationID: "conversation", ThroughSequence: 1, PrefixDigest: digest,
		ItemID: "spec-approval", ItemVersion: 2, SpecArtifactID: "specification", PolicyArtifactID: "policy",
		InputArtifactIDs: []domain.ArtifactID{"specification", "spec-discussion-command"},
	}
	for _, fixture := range []struct {
		kind    string
		key     string
		payload any
		reader  store.BackupPayloadDigestExtractor
	}{
		{engine.KindOperatorFeedbackInvocationRequested, string(feedback.InvocationID), feedback, engine.OperatorFeedbackInvocationBackupPayloadDigests},
		{domain.PublicationSuccessorKind, successor.Key(), successor, engine.PublicationSuccessorBackupPayloadDigests},
		{engine.KindRemediationInvocationRequested, string(remediation.InvocationID), remediation, engine.RemediationInvocationBackupPayloadDigests},
		{domain.PublicationContinuationRequestedKind, continuation.Key(), continuation, engine.PublicationContinuationBackupPayloadDigests},
		{engine.KindSpecificationDiscussionRequested, string(discussion.InvocationID), discussion, engine.SpecificationDiscussionBackupPayloadDigests},
	} {
		t.Run(fixture.kind, func(t *testing.T) {
			payload, err := json.Marshal(fixture.payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, scenario := range []string{"valid", "malformed payload", "wrong key", "unknown kind"} {
				t.Run(scenario, func(t *testing.T) {
					entry := store.QueueEntry{Kind: fixture.kind, IdempotencyKey: fixture.key, Payload: payload}
					switch scenario {
					case "malformed payload":
						entry.Payload = []byte(`{}`)
					case "wrong key":
						entry.IdempotencyKey += "-wrong"
					case "unknown kind":
						entry.Kind = "unsupported.workflow"
					}
					path := filepath.Join(t.TempDir(), "freeside.db")
					blobs, err := realRunBlobStore(path, false)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := blobs.Put(digest, strings.NewReader(input)); err != nil {
						t.Fatal(err)
					}
					files, err := realRunBackupFiles(path, false)
					if err != nil {
						t.Fatal(err)
					}
					producerReader := fixture.reader
					if scenario != "valid" {
						// Seal a test-owned adversarial checkpoint. Encryption alone
						// must not make an unsupported or malformed row trustworthy.
						producerReader = func(store.QueueEntry) ([]domain.Digest, error) { return nil, nil }
					}
					source, err := files.NewCheckpointHealthSource(blobs, nil,
						map[string]store.BackupPayloadDigestExtractor{entry.Kind: producerReader})
					if err != nil {
						t.Fatal(err)
					}
					writer := storetest.Open(t, path, store.Options{BackupHealthSource: source})
					if err := writer.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
						_, _, err := tx.EnqueueOutbox(t.Context(), entry.IdempotencyKey, entry.Kind, entry.Payload)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					producer, err := files.NewProducer(writer)
					if err != nil {
						t.Fatal(err)
					}
					if err := producer.Maintain(t.Context()); err != nil {
						t.Fatalf("produce encrypted checkpoint: %v", err)
					}
					producerHealth, err := writer.BackupHealth(t.Context())
					if err != nil || producerHealth.RequireHealthy() != nil {
						t.Fatalf("producer health = %+v, error = %v", producerHealth, err)
					}
					// Use the final verifier's existing-file and read-only paths,
					// with its own validating registry, beside the active writer.
					verifierFiles, err := realRunBackupFiles(path, true)
					if err != nil {
						t.Fatal(err)
					}
					verifierSource, err := verifierFiles.NewCheckpointHealthSource(blobs, nil, realRunBackupPayloadExtractors())
					if err != nil {
						t.Fatal(err)
					}
					verifier, err := realRunOpenStore(t.Context(), path, store.Options{BackupHealthSource: verifierSource}, true)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = verifier.Close() })
					health, err := verifier.BackupHealth(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "valid" {
						if err := health.RequireHealthy(); err != nil {
							t.Fatalf("final admission rejected supported workflow: %v", err)
						}
					} else if health.RequireHealthy() == nil {
						t.Fatalf("final admission accepted %s", scenario)
					}
				})
			}
		})
	}
}
