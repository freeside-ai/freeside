package fake

import (
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// The permanent fakes hold committed results in maps and hand them back on
// every redelivery. A result's slice-backed fields would otherwise alias the
// caller's (and the fake's) backing array, so a caller that mutated a
// delivered slice would mutate the committed snapshot and see a different
// value on the next Collect/Poll (issue #35). Cloning the slice fields when a
// result is scripted, committed, and returned keeps every redelivery a
// value-identical immutable snapshot regardless of caller behavior.
//
// domain.Digest is a string, so a StageResult's slice-backed fields fully
// detach with a one-level slices.Clone. A domain.Finding is otherwise scalar
// but carries an optional *FindingLocation, a nested reference field: a
// one-level clone would alias it, so a caller mutating a delivered finding's
// location would reach the committed snapshot. cloneReviewResult deep-copies
// each present location. slices.Clone preserves nil, so the serialized form
// (and the acceptor's byte comparison) is unchanged.

func cloneStageResult(r exec.StageResult) exec.StageResult {
	r.Artifacts = slices.Clone(r.Artifacts)
	return r
}

func cloneReviewResult(r exec.ReviewResult) exec.ReviewResult {
	r.Findings = slices.Clone(r.Findings)
	for i := range r.Findings {
		if loc := r.Findings[i].Location; loc != nil {
			clone := *loc
			r.Findings[i].Location = &clone
		}
	}
	return r
}

func cloneReviewRequest(r exec.ReviewRequest) exec.ReviewRequest {
	r.Verification.ArtifactDigests = slices.Clone(r.Verification.ArtifactDigests)
	r.Instructions.RepositorySources = slices.Clone(r.Instructions.RepositorySources)
	if r.Instructions.HostDigest != nil {
		digest := *r.Instructions.HostDigest
		r.Instructions.HostDigest = &digest
	}
	return r
}
