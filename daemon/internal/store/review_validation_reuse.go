package store

import "database/sql"

// reviewValidationState retains only successful cross-row checks, never decoded
// values. Store.Read owns marks for its snapshot; a disposition load in a write
// transaction installs them only until that read-only call returns. Counts track
// checks actually executed, so tests can bound work without timing assertions.
type reviewValidationState struct {
	marks                                               *reviewValidationMarks
	scopeChecks, adjudicationChecks, reviewRecordChecks int
}

type reviewValidationMarks struct {
	adjudications map[adjudicationValidationKey]bool
	reviews       map[reviewValidationKey]bool
	scopes        map[dispositionValidationKey]bool
	bindings      map[dispositionValidationKey]bool
}

func newReviewValidationMarks() *reviewValidationMarks {
	return &reviewValidationMarks{
		adjudications: make(map[adjudicationValidationKey]bool),
		reviews:       make(map[reviewValidationKey]bool),
		scopes:        make(map[dispositionValidationKey]bool),
		bindings:      make(map[dispositionValidationKey]bool),
	}
}

type adjudicationValidationKey struct {
	runID                                                 string
	round, revision                                       int
	predecessorDigest                                     sql.NullString
	contentDigest, findingBatchDigest, approvedSpecDigest string
	instructionSnapshotDigest, resolvedPolicyDigest       string
	createdAt, bodyDigest, body, path                     string
}

func (row findingAdjudicationRow) validationKey(path string) adjudicationValidationKey {
	return adjudicationValidationKey{
		runID: row.runID, round: row.round, revision: row.revision,
		predecessorDigest: row.predecessorDigest, contentDigest: row.contentDigest,
		findingBatchDigest: row.findingBatchDigest, approvedSpecDigest: row.approvedSpecDigest,
		instructionSnapshotDigest: row.instructionSnapshotDigest, resolvedPolicyDigest: row.resolvedPolicyDigest,
		createdAt: row.createdAt, bodyDigest: row.bodyDigest, body: string(row.body), path: path,
	}
}

type reviewValidationKey struct {
	invocationID, runID                                      string
	round                                                    int
	baseSHA, headSHA, outcome, completedAt, bodyDigest, body string
}

type dispositionValidationKey struct {
	findingID, runID                                                          string
	round                                                                     int
	class, reason, remediationInvocationID, createdAt, bodyDigest, body, path string
}
