package domain

import (
	"fmt"
	"time"
	"unicode/utf8"
)

// WorkUnitRebaseProofDetailLimit bounds, in bytes, a refused proof's Detail.
// The detail can quote a commit subject from a branch anyone with push access
// can write, so it is cut to this size before it is stored.
const WorkUnitRebaseProofDetailLimit = 512

// WorkUnitRebaseProof is the write-once verdict on whether a pull request head
// someone else pushed is the reviewed candidate rebased onto the base the last
// review used (issue #2129). A lander that requires up-to-date branches
// rebases and force-pushes before it merges, so the merged head is never the
// one Freeside reviewed. The daemon proves the rebase instead of reviewing the
// merged head: the head descends from ReviewedBaseSHA, every reviewed commit
// reappears in order with the same patch, file contents, and message and no
// merge commit is added, and its tree is ReviewedTreeSHA, the prospective
// merge the review covered.
//
// A proven row moves the head the completion evaluator compares to
// ProvenHeadSHA, and nothing else: it re-earns no readiness and admits no
// remediation. Proven is a decoded trust bit, so the store honors a row only
// while ReviewedBaseSHA and ReviewedHeadSHA are still the latest review's and
// ReviewedHeadSHA is still the published head. A refused row records why, so
// a merge of that head forecloses the unit instead of waiting on a proof.
type WorkUnitRebaseProof struct {
	UnitID       WorkUnitID `json:"unit_id"`
	RepositoryID int64      `json:"repository_id"`
	PRNumber     int        `json:"pr_number"`
	// ReviewedBaseSHA and ReviewedHeadSHA are the pair the last clean review
	// covered.
	ReviewedBaseSHA string `json:"reviewed_base_sha"`
	ReviewedHeadSHA string `json:"reviewed_head_sha"`
	// ProvenHeadSHA is the head the proof evaluated, proven or not.
	ProvenHeadSHA string `json:"proven_head_sha"`
	// ReviewedTreeSHA is the tree of the reviewed pair's prospective merge. A
	// refusal reached before the merge was computed leaves it empty.
	ReviewedTreeSHA string `json:"reviewed_tree_sha,omitempty"`
	Proven          bool   `json:"proven"`
	// Detail is the first reason the proof failed, empty when Proven. It is
	// bounded text the daemon wrote, quoting at most one range-diff line.
	Detail     string    `json:"detail,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Validate reports whether the proof is well-formed: a proven head names the
// tree it matched and no failure, and a refused head names its failure.
func (p WorkUnitRebaseProof) Validate() error {
	if p.UnitID == "" {
		return fmt.Errorf("work unit rebase proof unit_id: %w", ErrEmptyID)
	}
	for name, v := range map[string]string{
		"reviewed_base_sha": p.ReviewedBaseSHA, "reviewed_head_sha": p.ReviewedHeadSHA,
		"proven_head_sha": p.ProvenHeadSHA,
	} {
		if v == "" {
			return fmt.Errorf("work unit rebase proof %s %s: %w", p.UnitID, name, ErrEmptyField)
		}
	}
	if p.RepositoryID <= 0 {
		return fmt.Errorf("work unit rebase proof %s repository_id %d: %w",
			p.UnitID, p.RepositoryID, ErrNonPositive)
	}
	if p.PRNumber <= 0 {
		return fmt.Errorf("work unit rebase proof %s pr_number %d: %w", p.UnitID, p.PRNumber, ErrNonPositive)
	}
	if p.ProvenHeadSHA == p.ReviewedHeadSHA {
		return fmt.Errorf("work unit rebase proof %s evaluates the reviewed head itself: %w",
			p.UnitID, ErrRebaseProofInconsistent)
	}
	if p.Proven && (p.ReviewedTreeSHA == "" || p.Detail != "") {
		return fmt.Errorf("work unit rebase proof %s proven with tree %q and detail %q: %w",
			p.UnitID, p.ReviewedTreeSHA, p.Detail, ErrRebaseProofInconsistent)
	}
	if !p.Proven && p.Detail == "" {
		return fmt.Errorf("work unit rebase proof %s refused without a detail: %w",
			p.UnitID, ErrRebaseProofInconsistent)
	}
	if len(p.Detail) > WorkUnitRebaseProofDetailLimit || !utf8.ValidString(p.Detail) {
		return fmt.Errorf("work unit rebase proof %s detail is not bounded UTF-8: %w",
			p.UnitID, ErrRebaseProofInconsistent)
	}
	if p.RecordedAt.IsZero() {
		return fmt.Errorf("work unit rebase proof %s recorded_at: %w", p.UnitID, ErrMissingTimestamp)
	}
	if p.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("work unit rebase proof %s recorded_at: %w", p.UnitID, ErrTimestampNotUTC)
	}
	return nil
}
