package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ReviewSourceFailure classifies an invocation failure at the source
// boundary. The engine persists the classification before it retries, raises
// attention, or returns a contradiction loudly.
type ReviewSourceFailure struct {
	Class domain.ReviewFailureClass
	Err   error
}

func (e *ReviewSourceFailure) Error() string {
	return fmt.Sprintf("review source %s failure: %v", e.Class, e.Err)
}

func (e *ReviewSourceFailure) Unwrap() error { return e.Err }

// ClassifyReviewSourceFailure returns the declared source class. Unclassified
// failures are contradictions, never guessed into a retryable bucket.
func ClassifyReviewSourceFailure(err error) domain.ReviewFailureClass {
	var failure *ReviewSourceFailure
	if errors.As(err, &failure) && failure.Class != "" {
		return failure.Class
	}
	return domain.ReviewFailureContradiction
}

// ReviewSource requests and reconciles external reviews (plan §5.3).
// Invocation-id-first like StageDriver: every operation is keyed by the
// daemon-generated id passed to RequestReview, and operations on an id never
// requested return ErrUnknownInvocation.
type ReviewSource interface {
	// RequestReview commits the review intent for the given head and begins
	// the review. A second request with the same id returns
	// ErrDuplicateStart (one committed intent per id, §5.3).
	RequestReview(ctx context.Context, id domain.InvocationID, req ReviewRequest) error
	// Inspect reports the review invocation's current lifecycle status.
	Inspect(ctx context.Context, id domain.InvocationID) (Status, error)
	// Poll returns the committed review result. It is idempotent: repeated
	// polls re-deliver the identical result, and accepting it at most once
	// is the caller's job. Before a result is committed it returns
	// ErrResultNotReady; if the review ended without committing one it
	// returns ErrNoResult.
	Poll(ctx context.Context, id domain.InvocationID) (ReviewResult, error)
	// Verify checks the committed result's freshness: it fails with
	// ErrStaleHead when the head the review actually ran against differs
	// from expectedHead (a review of a superseded head must never gate the
	// current one, §5.3).
	Verify(ctx context.Context, id domain.InvocationID, expectedBase, expectedHead string) error
}

// ReviewRequestAuthorityVerifier is the mandatory production extension that
// binds a reconstructed request to the engine's current verification
// checkpoint. The engine verifies twice per reconciliation: before Inspect,
// so a rewritten-but-valid persisted request fails closed before it can
// relaunch anything, and again before a delivered result may satisfy
// readiness. A verifier for an id with no persisted request returns
// ErrUnknownInvocation, which the engine treats as "nothing to gate yet".
// On a mismatch the source must reconcile what the original request
// already started before reporting the contradiction — abort a started
// invocation through its durable teardown protocol and reap any prepared
// workspace — because the engine terminalizes on the contradiction and
// never inspects the invocation again; a rejected request must not strand
// a credential-bearing topology. While that teardown is still converging
// the verifier reports a transient failure so the engine retries. An
// authenticated pre-authority request reports ErrLegacyReviewRequest after
// teardown so the engine can supersede it with a new round.
type ReviewRequestAuthorityVerifier interface {
	VerifyRequestAuthority(
		ctx context.Context, id domain.InvocationID, expected domain.Digest,
	) error
}

// ReviewRequestSupersessionVerifier recognizes a persisted request whose only
// authority change is an updated trusted instruction binding. It must tear
// down that request before returning ErrSupersededReviewRequest.
type ReviewRequestSupersessionVerifier interface {
	VerifyReviewRequestSupersession(ctx context.Context, id domain.InvocationID, expected ReviewRequest) error
}

// ReviewRequest is what a review source needs to review one exact candidate.
type ReviewRequest struct {
	RunID        domain.RunID               `json:"run_id"`
	Round        int                        `json:"round"`
	Repo         string                     `json:"repo"`
	RepositoryID int64                      `json:"repository_id"`
	BaseRef      string                     `json:"base_ref"`
	BaseSHA      string                     `json:"base_sha"`
	HeadSHA      string                     `json:"head_sha"`
	Workspace    string                     `json:"workspace"`
	Verification ReviewVerificationEvidence `json:"verification"`
	Instructions ReviewInstructionBinding   `json:"instructions"`
	RequestedAt  time.Time                  `json:"requested_at"`
}

// ReviewVerificationEvidence is the compact, content-addressed account of
// the clean verification that preceded a review pass. It gives the reviewer
// the verdict and exact evidence identities without any implementer
// transcript or reasoning history.
type ReviewVerificationEvidence struct {
	Outcome                domain.VerificationOutcome `json:"outcome"`
	RecipeDigest           domain.Digest              `json:"recipe_digest"`
	EvidenceSnapshotDigest domain.Digest              `json:"evidence_snapshot_digest"`
	ArtifactDigests        []domain.Digest            `json:"artifact_digests"`
}

func NewReviewVerificationEvidence(
	evidence ReviewVerificationEvidence,
) (ReviewVerificationEvidence, error) {
	evidence.ArtifactDigests = slices.Clone(evidence.ArtifactDigests)
	slices.Sort(evidence.ArtifactDigests)
	evidence.ArtifactDigests = slices.Compact(evidence.ArtifactDigests)
	if len(evidence.ArtifactDigests) == 0 {
		evidence.ArtifactDigests = nil
	}
	if err := evidence.Validate(); err != nil {
		return ReviewVerificationEvidence{}, err
	}
	return evidence, nil
}

func (e ReviewVerificationEvidence) Validate() error {
	if e.Outcome != domain.VerificationPassed {
		return fmt.Errorf("review verification outcome %q: %w", e.Outcome, domain.ErrInvalidOutcome)
	}
	for _, item := range []struct {
		name   string
		digest domain.Digest
	}{
		{"recipe_digest", e.RecipeDigest},
		{"evidence_snapshot_digest", e.EvidenceSnapshotDigest},
	} {
		if !contentaddr.Valid(string(item.digest)) {
			return fmt.Errorf("review verification %s %q: %w",
				item.name, item.digest, domain.ErrInvalidReviewCompletionEvidence)
		}
	}
	for i, digest := range e.ArtifactDigests {
		if !contentaddr.Valid(string(digest)) ||
			(i > 0 && digest <= e.ArtifactDigests[i-1]) {
			return fmt.Errorf("review verification artifact_digests[%d] %q: %w",
				i, digest, domain.ErrDigestsNotCanonical)
		}
	}
	return nil
}

func (r ReviewRequest) Validate() error {
	switch {
	case r.RunID == "":
		return fmt.Errorf("review request run_id: %w", domain.ErrEmptyID)
	case r.Round < 1:
		return fmt.Errorf("review request round %d: %w", r.Round, domain.ErrNonPositive)
	case r.Repo == "" || r.RepositoryID <= 0 || r.BaseRef == "" ||
		r.BaseSHA == "" || r.HeadSHA == "" || r.Workspace == "":
		return fmt.Errorf("review request candidate binding: %w", domain.ErrEmptyField)
	case r.RequestedAt.IsZero():
		return fmt.Errorf("review request requested_at: %w", domain.ErrMissingTimestamp)
	case r.RequestedAt.Location() != time.UTC:
		return fmt.Errorf("review request requested_at: %w", domain.ErrTimestampNotUTC)
	}
	return errors.Join(r.Verification.Validate(), r.Instructions.Validate())
}

// AuthorityDigest binds the reviewer to every request field that can change
// which candidate or verification authority it reviews. Request time is an
// operational observation, not delegated authority.
func (r ReviewRequest) AuthorityDigest() (domain.Digest, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Version      string                     `json:"version"`
		RunID        domain.RunID               `json:"run_id"`
		Round        int                        `json:"round"`
		Repo         string                     `json:"repo"`
		RepositoryID int64                      `json:"repository_id"`
		BaseRef      string                     `json:"base_ref"`
		BaseSHA      string                     `json:"base_sha"`
		HeadSHA      string                     `json:"head_sha"`
		Workspace    string                     `json:"workspace"`
		Verification ReviewVerificationEvidence `json:"verification"`
		Instructions ReviewInstructionBinding   `json:"instructions"`
	}{
		Version: "review-request-authority-v2", RunID: r.RunID, Round: r.Round,
		Repo: r.Repo, RepositoryID: r.RepositoryID, BaseRef: r.BaseRef,
		BaseSHA: r.BaseSHA, HeadSHA: r.HeadSHA, Workspace: r.Workspace,
		Verification: r.Verification, Instructions: r.Instructions,
	})
	if err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Sum(body)), nil
}

// ReviewResult is the committed outcome of a review invocation: the
// serialized contract the store persists and the engine accepts (at most
// once, §5.3). A clean pass is an empty Findings list; there is no separate
// verdict field to drift from it.
type ReviewResult struct {
	InvocationID        domain.InvocationID `json:"invocation_id"`
	BaseSHA             string              `json:"base_sha"`
	HeadSHA             string              `json:"head_sha"`
	Provider            string              `json:"provider"`
	ModelConfiguration  string              `json:"model_configuration"`
	ConfigurationDigest domain.Digest       `json:"configuration_digest"`
	InstructionDigest   domain.Digest       `json:"instruction_digest"`
	CostOwner           string              `json:"cost_owner"`
	CompletedAt         time.Time           `json:"completed_at"`
	CompletionEvidence  domain.Digest       `json:"completion_evidence"`
	Findings            []domain.Finding    `json:"findings"`
}

// Validate reports whether the result is well-formed: reconcilable
// (non-empty invocation id), head-bound (a review unbindable to a head can
// never pass Verify), and carrying only well-formed findings. It is the
// deserialization backstop for results reconstructed from the store.
func (r ReviewResult) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("review result invocation_id: %w", domain.ErrEmptyID)
	}
	if r.HeadSHA == "" {
		return fmt.Errorf("review result head_sha: %w", domain.ErrEmptyField)
	}
	if r.BaseSHA == "" || r.Provider == "" || r.ModelConfiguration == "" || r.CostOwner == "" ||
		!contentaddr.Valid(string(r.ConfigurationDigest)) ||
		!contentaddr.Valid(string(r.InstructionDigest)) {
		return fmt.Errorf("review result provenance: %w", domain.ErrEmptyField)
	}
	if !contentaddr.Valid(string(r.CompletionEvidence)) {
		return fmt.Errorf("review result completion evidence %q: %w",
			r.CompletionEvidence, domain.ErrInvalidReviewCompletionEvidence)
	}
	if r.CompletedAt.IsZero() {
		return fmt.Errorf("review result completed_at: %w", domain.ErrMissingTimestamp)
	}
	if r.CompletedAt.Location() != time.UTC {
		return fmt.Errorf("review result completed_at: %w", domain.ErrTimestampNotUTC)
	}
	seen := make(map[domain.FindingID]struct{}, len(r.Findings))
	for i, f := range r.Findings {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("review result findings[%d]: %w", i, err)
		}
		if f.Source == "" || f.Severity == "" || f.Message == "" {
			return fmt.Errorf("review result findings[%d] attribution: %w", i, domain.ErrEmptyField)
		}
		if _, duplicate := seen[f.ID]; duplicate {
			return fmt.Errorf("review result findings[%d] duplicates %q: %w",
				i, f.ID, domain.ErrFindingsNotCanonical)
		}
		seen[f.ID] = struct{}{}
	}
	return nil
}
