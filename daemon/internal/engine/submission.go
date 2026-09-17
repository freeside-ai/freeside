package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// This file holds the submission primitives shared by the operator CLI
// (freesided submit) and the client command surface (signet's task-submission
// handler, injected through a TaskSubmitter): the run-id derivation, the
// content-addressed input-artifact registration, and the declared-path
// boundary gate. They live here, above both callers, because the engine
// already owns SubmitSpecificationRun and the client surface cannot reach the
// CLI (and signet cannot import the engine). Behaviour is unchanged from the
// CLI originals they replace.

// SplitNonEmpty splits a comma-separated value into its non-empty members, so
// an unset value yields no members rather than one empty one.
func SplitNonEmpty(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ExplicitAllowedPaths requires every pattern to name a literal top-level
// repository path. A leading glob segment (for example **/*, */**, or ?*) can
// be semantically match-all even when it is not the literal "**"; such a
// pattern does not declare a containment boundary.
func ExplicitAllowedPaths(patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	if err := importer.ValidatePathPatterns(patterns); err != nil {
		return false
	}
	for _, pattern := range patterns {
		first, _, _ := strings.Cut(pattern, "/")
		if first == "" || first == "." || first == ".." ||
			strings.ContainsAny(first, `*?[\`) {
			return false
		}
	}
	return true
}

// SubmittedPathBoundary refuses a policy that names no enforceable declared
// paths. It is the submission-time half of the runner's start-time path gate:
// that gate compares the policy against one daemon's configuration and so must
// hold rather than fail, which would leave a policy carrying no usable
// boundary at all held forever. Refusing it at submission keeps that state out
// of the store.
func SubmittedPathBoundary(policy domain.ResolvedPolicy) error {
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		if !ExplicitAllowedPaths(SplitNonEmpty(key.Value)) {
			return fmt.Errorf(
				"resolved policy paths %q are not an explicit declared-path allowlist: %w",
				key.Value, domain.ErrPathBoundaryMismatch,
			)
		}
		return nil
	}
	return fmt.Errorf(
		"resolved policy declares no paths key: %w", domain.ErrPathBoundaryMismatch,
	)
}

// DeclaredPathScope extracts the resolved policy's paths key as the unit's
// declared path scope, through the domain's single canonical definition — the
// same one the store's declaration re-gate re-derives with, so the recorded
// scope and the re-gate can never disagree. The submission gate
// (SubmittedPathBoundary) has already refused a policy without an explicit
// allowlist, so a declared unit always carries the scope the runner enforces.
func DeclaredPathScope(keys []domain.PolicyKey) []string {
	return domain.CanonicalDeclaredPaths(domain.ResolvedPolicy{Keys: keys})
}

// SubmissionRunID derives an implementation run id from the immutable bindings
// a submission is prepared against, so only an exact resubmission converges:
// shared specification bytes in another project, under another policy, with
// different reviewer-facing metadata, or under a different work-unit
// declaration remain distinct implementation runs.
func SubmissionRunID(
	projectID domain.ProjectID, specDigest, policyDigest, publicationDigest, workUnitDigest domain.Digest,
) domain.RunID {
	bindings := string(projectID) + "\x00" + string(specDigest) + "\x00" +
		string(policyDigest) + "\x00" + string(publicationDigest)
	if workUnitDigest != "" {
		bindings += "\x00" + string(workUnitDigest)
	}
	sum := sha256.Sum256([]byte(bindings))
	return domain.RunID("run-" + hex.EncodeToString(sum[:]))
}

// ManualSubmissionRunID includes the deliberate submission identity alongside
// the immutable execution bindings. SubmissionRunID remains the legacy formula.
func ManualSubmissionRunID(identity string, projectID domain.ProjectID, source, policy, publication, workUnit domain.Digest) domain.RunID {
	legacy := SubmissionRunID(projectID, source, policy, publication, workUnit)
	sum := sha256.Sum256([]byte("freeside.manual-submission/v1\x00" + identity + "\x00" + string(legacy)))
	return domain.RunID("run-" + hex.EncodeToString(sum[:]))
}

// SubmissionArtifact is the digest-addressed registration of one submitted
// input. Identity and provenance are both content-derived (never
// run-derived), so two runs submitting the same bytes converge on one
// write-once artifact row instead of conflicting; the daemon-produced
// provenance carries no recipe, so the artifact is never publish-eligible.
//
// The evidence metadata's created_at is the host clock at registration, so a
// re-registration of the same content produces a byte-different row; callers
// that persist go through RegisterSubmissionArtifact, which keeps the existing
// write-once row and never re-puts, preserving the convergence guarantee.
func SubmissionArtifact(
	role domain.ArtifactKind, digest domain.Digest, mediaType domain.EvidenceMediaType, sizeBytes int64,
) (domain.Artifact, error) {
	hexDigits := string(digest[len("sha256:"):])
	return domain.NewArtifact(domain.ArtifactInput{
		ID:     domain.ArtifactID("artifact-" + string(role) + "-" + hexDigits),
		Type:   role,
		Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: domain.InvocationID("submit-" + string(role) + "-" + hexDigits),
			HeadBinding:          domain.HeadIndependent,
			SensitivityClass:     domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: mediaType, SizeBytes: sizeBytes, CreatedAt: time.Now().UTC(),
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, nil)
}

// RegisterSubmissionArtifact write-once registers a content-addressed input
// artifact idempotently. The artifact ID embeds the content digest, so an
// existing row with the same ID names the same bytes and provenance; only the
// recorded created_at could differ across a replayed submit or a repeated
// reconcile pass, so the original row stays authoritative and is never re-put
// (a plain PutArtifact would reject the byte-different re-registration). A row
// whose digest diverges from the submission is a restored or corrupted
// inconsistency, not this input, so it is refused fail-closed rather than
// silently binding the run to different bytes than were submitted.
func RegisterSubmissionArtifact(ctx context.Context, tx *store.WriteTx, a domain.Artifact) error {
	existing, err := tx.GetArtifact(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		return tx.PutArtifact(ctx, a)
	}
	if err != nil {
		return err
	}
	if existing.Digest != a.Digest {
		return fmt.Errorf("artifact %s digest %s, existing row %s: %w",
			a.ID, a.Digest, existing.Digest, store.ErrImmutableConflict)
	}
	return nil
}
