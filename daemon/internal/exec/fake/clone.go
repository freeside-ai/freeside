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
// domain.Digest is a string and domain.Finding is all scalars and time.Time,
// so a one-level slices.Clone fully detaches the result; there are no nested
// reference fields to deep-copy. slices.Clone preserves nil, so the
// serialized form (and the acceptor's byte comparison) is unchanged.

func cloneStageResult(r exec.StageResult) exec.StageResult {
	r.Artifacts = slices.Clone(r.Artifacts)
	return r
}

func cloneReviewResult(r exec.ReviewResult) exec.ReviewResult {
	r.Findings = slices.Clone(r.Findings)
	return r
}
