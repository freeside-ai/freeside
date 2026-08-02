package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// Work-unit capture records (plan §5.18, issue #443): the explicit
// declarations and first-party observations the 1B.2 frontier projection
// will derive from. Capture is recording only — nothing in the daemon
// computes over these records yet — but unlike the run-observation
// projection (observation.go) they are the authority that later derivation
// stands on, so every shape validates like a contract type and the store
// re-gates it at reconstruction. Every field is an explicit declaration or
// a daemon/forge fact; no inferred parallelism, guessed scope, or heuristic
// completion is representable here by design.

// CompletionCriterionKind names the declared rule under which a merge marks
// the unit done (§5.18: an exact daemon-recorded binding and criterion;
// partial, stacked, or related merges never complete units). The zero value
// "" is invalid by design.
type CompletionCriterionKind string

const (
	// CompletionBoundIssueClosedByMergedPR: the declared bound issue is
	// closed by exactly the bound PR's merge commit (the issue `closed`
	// event's commit id equals the merge commit SHA).
	CompletionBoundIssueClosedByMergedPR CompletionCriterionKind = "bound_issue_closed_by_merged_pr"
	// CompletionBoundPRMerged: the bound PR is merged into its admitted
	// base ref; the unit declares no issue-closure requirement.
	CompletionBoundPRMerged CompletionCriterionKind = "bound_pr_merged"
)

// AllCompletionCriterionKinds is the single registration point for
// completion criteria.
var AllCompletionCriterionKinds = []CompletionCriterionKind{
	CompletionBoundIssueClosedByMergedPR,
	CompletionBoundPRMerged,
}

func (k CompletionCriterionKind) valid() bool {
	switch k {
	case CompletionBoundIssueClosedByMergedPR, CompletionBoundPRMerged:
		return true
	default:
		return false
	}
}

// requiresBoundIssue dispatches the criterion's declaration contract: which
// kinds need the bound-issue coordinate to be evaluable at all.
func (k CompletionCriterionKind) requiresBoundIssue() bool {
	switch k {
	case CompletionBoundIssueClosedByMergedPR:
		return true
	case CompletionBoundPRMerged:
		return false
	}
	return false
}

// PullRequestState is the forge's observed pull-request lifecycle state.
// The zero value "" is invalid by design.
type PullRequestState string

const (
	PullRequestOpen   PullRequestState = "open"
	PullRequestClosed PullRequestState = "closed"
)

// AllPullRequestStates is the single registration point for PR states.
var AllPullRequestStates = []PullRequestState{PullRequestOpen, PullRequestClosed}

func (s PullRequestState) valid() bool {
	switch s {
	case PullRequestOpen, PullRequestClosed:
		return true
	default:
		return false
	}
}

// IssueState is the forge's observed issue lifecycle state. The zero value
// "" is invalid by design.
type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

// AllIssueStates is the single registration point for issue states.
var AllIssueStates = []IssueState{IssueOpen, IssueClosed}

func (s IssueState) valid() bool {
	switch s {
	case IssueOpen, IssueClosed:
		return true
	default:
		return false
	}
}

// WorkUnitIDForRun derives the unit identity from its run: one work unit
// per run until an intake that outlives a single run exists (the capture
// decision note's revisit condition).
func WorkUnitIDForRun(runID RunID) WorkUnitID {
	return WorkUnitID("workunit-" + string(runID))
}

// CanonicalDeclaredPaths derives the §5.18 declared path scope from a
// resolved policy's comma-separated paths key, in canonical (sorted,
// deduplicated) order; nil when the policy declares no paths. It is the
// single definition intake and the store's declaration re-gate share, so
// the recorded scope can never drift from the boundary the runner
// enforces.
func CanonicalDeclaredPaths(policy ResolvedPolicy) []string {
	for _, key := range policy.Keys {
		if key.Key != "paths" {
			continue
		}
		var paths []string
		for _, part := range strings.Split(key.Value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				paths = append(paths, trimmed)
			}
		}
		slices.Sort(paths)
		return slices.Compact(paths)
	}
	return nil
}

// WorkUnitDeclarationInput is the caller-supplied half of a declaration:
// exactly the coordinates the operator states at submission. The unit id
// and declaration instant are daemon-assigned and deliberately absent, so
// no input path can set them.
type WorkUnitDeclarationInput struct {
	// CompletionCriterion is the declared rule under which a merge marks
	// this unit done.
	CompletionCriterion CompletionCriterionKind `json:"completion_criterion"`
	// BoundIssue is the tracker issue number this unit is bound to;
	// required by criteria that evaluate issue closure, optional otherwise
	// (a unit may reference an issue it deliberately leaves open).
	BoundIssue *int `json:"bound_issue,omitempty"`
	// DependsOnIssues are the declared dependency edges, as tracker issue
	// numbers, in canonical (ascending, deduplicated) order.
	DependsOnIssues []int `json:"depends_on_issues,omitempty"`
	// DeclaredPaths is the unit's declared path scope, in canonical
	// (sorted, deduplicated) order; empty means the unit declared no scope,
	// which the projection must treat as unknown, never as unrestricted.
	DeclaredPaths []string `json:"declared_paths,omitempty"`
	// ContractSerialized declares the unit as serialized shared-contract
	// work.
	ContractSerialized bool `json:"contract_serialized"`
}

// WorkUnitDeclaration is the captured declaration record: the input verbatim
// plus the daemon-assigned identity, run binding, and declaration instant.
type WorkUnitDeclaration struct {
	ID                  WorkUnitID              `json:"id"`
	RunID               RunID                   `json:"run_id"`
	ProjectID           ProjectID               `json:"project_id"`
	CompletionCriterion CompletionCriterionKind `json:"completion_criterion"`
	BoundIssue          *int                    `json:"bound_issue,omitempty"`
	DependsOnIssues     []int                   `json:"depends_on_issues,omitempty"`
	DeclaredPaths       []string                `json:"declared_paths,omitempty"`
	ContractSerialized  bool                    `json:"contract_serialized"`
	DeclaredAt          time.Time               `json:"declared_at"`
}

// Validate reports whether the declaration is well-formed: identities and
// instant present, the criterion registered with its bound-issue contract
// satisfied, and both declared collections canonical, so byte-identical
// replay convergence (the store's write-once put) cannot be defeated by
// reordering.
func (d WorkUnitDeclaration) Validate() error {
	if d.ID == "" {
		return fmt.Errorf("work unit id: %w", ErrEmptyID)
	}
	if d.RunID == "" {
		return fmt.Errorf("work unit %s run_id: %w", d.ID, ErrEmptyID)
	}
	if d.ID != WorkUnitIDForRun(d.RunID) {
		return fmt.Errorf("work unit id %q is not derived from run %q: %w",
			d.ID, d.RunID, ErrWorkUnitInconsistent)
	}
	if d.ProjectID == "" {
		return fmt.Errorf("work unit %s project_id: %w", d.ID, ErrEmptyID)
	}
	if !d.CompletionCriterion.valid() {
		return fmt.Errorf("work unit %s completion_criterion %q: %w",
			d.ID, d.CompletionCriterion, ErrInvalidCompletionCriterion)
	}
	if d.CompletionCriterion.requiresBoundIssue() && d.BoundIssue == nil {
		return fmt.Errorf("work unit %s criterion %s without bound_issue: %w",
			d.ID, d.CompletionCriterion, ErrWorkUnitInconsistent)
	}
	if d.BoundIssue != nil && *d.BoundIssue <= 0 {
		return fmt.Errorf("work unit %s bound_issue %d: %w", d.ID, *d.BoundIssue, ErrNonPositive)
	}
	for _, n := range d.DependsOnIssues {
		if n <= 0 {
			return fmt.Errorf("work unit %s depends_on_issues %d: %w", d.ID, n, ErrNonPositive)
		}
	}
	if !slices.IsSorted(d.DependsOnIssues) || hasAdjacentDuplicate(d.DependsOnIssues) {
		return fmt.Errorf("work unit %s depends_on_issues: %w", d.ID, ErrDependenciesNotCanonical)
	}
	for _, p := range d.DeclaredPaths {
		if p == "" {
			return fmt.Errorf("work unit %s declared_paths entry: %w", d.ID, ErrEmptyField)
		}
	}
	if !slices.IsSorted(d.DeclaredPaths) || hasAdjacentDuplicate(d.DeclaredPaths) {
		return fmt.Errorf("work unit %s declared_paths: %w", d.ID, ErrDeclaredPathsNotCanonical)
	}
	if d.DeclaredAt.IsZero() {
		return fmt.Errorf("work unit %s declared_at: %w", d.ID, ErrMissingTimestamp)
	}
	if d.DeclaredAt.Location() != time.UTC {
		return fmt.Errorf("work unit %s declared_at: %w", d.ID, ErrTimestampNotUTC)
	}
	return nil
}

// hasAdjacentDuplicate reports a repeated element in an already-sorted
// slice; combined with slices.IsSorted it checks canonical order without
// allocating.
func hasAdjacentDuplicate[T comparable](s []T) bool {
	for i := 1; i < len(s); i++ {
		if s[i] == s[i-1] {
			return true
		}
	}
	return false
}

// NewWorkUnitDeclaration builds the captured record from the operator's
// input and the daemon-side run coordinates. It is the single trusted
// constructor: the unit id derives from the run, and the instant is stamped
// in UTC here so identity-bearing time never arrives from a caller's clock
// zone.
func NewWorkUnitDeclaration(
	in WorkUnitDeclarationInput, runID RunID, projectID ProjectID, declaredAt time.Time,
) (WorkUnitDeclaration, error) {
	d := WorkUnitDeclaration{
		ID: WorkUnitIDForRun(runID), RunID: runID, ProjectID: projectID,
		CompletionCriterion: in.CompletionCriterion,
		BoundIssue:          in.BoundIssue,
		DependsOnIssues:     in.DependsOnIssues,
		DeclaredPaths:       in.DeclaredPaths,
		ContractSerialized:  in.ContractSerialized,
		DeclaredAt:          declaredAt.UTC(),
	}
	// Empty collections normalize to nil: the omitempty round-trip through
	// the store decodes an empty list as nil, and a constructor that kept
	// the caller's empty non-nil slice would make a legitimate replay
	// compare unequal against its own stored declaration.
	if len(d.DependsOnIssues) == 0 {
		d.DependsOnIssues = nil
	}
	if len(d.DeclaredPaths) == 0 {
		d.DeclaredPaths = nil
	}
	if err := d.Validate(); err != nil {
		return WorkUnitDeclaration{}, err
	}
	return d, nil
}

// WorkUnitPRBinding is the exact daemon-recorded binding of a unit to its
// published pull request: every coordinate is a first-party fact (the
// admitted BaseRevision, the publish result's PR number, the imported head
// commit), never an observation of external prose.
type WorkUnitPRBinding struct {
	UnitID WorkUnitID `json:"unit_id"`
	Repo   string     `json:"repo"`
	// RepositoryID is the forge's canonical numeric identity for Repo (the
	// BaseRevision convention): completion matching keys on it, so a
	// transferred or reused name can never satisfy another repository's
	// binding.
	RepositoryID int64 `json:"repository_id"`
	PRNumber     int   `json:"pr_number"`
	// BaseRef is the admitted integration base; a merge into any other base
	// (a stacked or retargeted PR) never completes the unit.
	BaseRef    string    `json:"base_ref"`
	HeadSHA    string    `json:"head_sha"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ReadyItemPRBinding is the daemon-recorded resource identity behind one
// ready_for_final_review item. Unlike WorkUnitPRBinding it exists for every
// published ready item, including runs with no optional work-unit declaration,
// so active-resource reconciliation can resume from durable state after a
// restart without deriving coordinates from presentation text.
type ReadyItemPRBinding struct {
	ItemID                  ItemID       `json:"item_id"`
	RunID                   RunID        `json:"run_id"`
	ProducingInvocationID   InvocationID `json:"producing_invocation_id"`
	PublicationInvocationID InvocationID `json:"publication_invocation_id"`
	PublicationIdentity     Digest       `json:"publication_identity"`
	Repo                    string       `json:"repo"`
	RepositoryID            int64        `json:"repository_id"`
	PRNumber                int          `json:"pr_number"`
	BaseRef                 string       `json:"base_ref"`
	HeadSHA                 string       `json:"head_sha"`
	RecordedAt              time.Time    `json:"recorded_at"`
}

// Validate reports whether the ready resource binding is well-formed.
func (b ReadyItemPRBinding) Validate() error {
	if b.ItemID == "" {
		return fmt.Errorf("ready item pr binding item_id: %w", ErrEmptyID)
	}
	if b.RunID == "" {
		return fmt.Errorf("ready item pr binding run_id: %w", ErrEmptyID)
	}
	if b.ProducingInvocationID == "" {
		return fmt.Errorf("ready item pr binding producing_invocation_id: %w", ErrEmptyID)
	}
	if b.PublicationInvocationID == "" {
		return fmt.Errorf("ready item pr binding publication_invocation_id: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(b.PublicationIdentity)) {
		return fmt.Errorf("ready item pr binding publication_identity %q: %w",
			b.PublicationIdentity, ErrEmptyField)
	}
	for name, value := range map[string]string{
		"repo": b.Repo, "base_ref": b.BaseRef, "head_sha": b.HeadSHA,
	} {
		if value == "" {
			return fmt.Errorf("ready item pr binding %s %s: %w", b.ItemID, name, ErrEmptyField)
		}
	}
	if b.RepositoryID <= 0 {
		return fmt.Errorf("ready item pr binding %s repository_id %d: %w",
			b.ItemID, b.RepositoryID, ErrNonPositive)
	}
	if b.PRNumber <= 0 {
		return fmt.Errorf("ready item pr binding %s pr_number %d: %w",
			b.ItemID, b.PRNumber, ErrNonPositive)
	}
	if b.RecordedAt.IsZero() {
		return fmt.Errorf("ready item pr binding %s recorded_at: %w", b.ItemID, ErrMissingTimestamp)
	}
	if b.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("ready item pr binding %s recorded_at: %w", b.ItemID, ErrTimestampNotUTC)
	}
	return nil
}

// Validate reports whether the binding is well-formed.
func (b WorkUnitPRBinding) Validate() error {
	if b.UnitID == "" {
		return fmt.Errorf("work unit pr binding unit_id: %w", ErrEmptyID)
	}
	for name, v := range map[string]string{
		"repo": b.Repo, "base_ref": b.BaseRef, "head_sha": b.HeadSHA,
	} {
		if v == "" {
			return fmt.Errorf("work unit pr binding %s %s: %w", b.UnitID, name, ErrEmptyField)
		}
	}
	if b.RepositoryID <= 0 {
		return fmt.Errorf("work unit pr binding %s repository_id %d: %w",
			b.UnitID, b.RepositoryID, ErrNonPositive)
	}
	if b.PRNumber <= 0 {
		return fmt.Errorf("work unit pr binding %s pr_number %d: %w",
			b.UnitID, b.PRNumber, ErrNonPositive)
	}
	if b.RecordedAt.IsZero() {
		return fmt.Errorf("work unit pr binding %s recorded_at: %w", b.UnitID, ErrMissingTimestamp)
	}
	if b.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("work unit pr binding %s recorded_at: %w", b.UnitID, ErrTimestampNotUTC)
	}
	return nil
}

// PullMergeFact is one observation of a bound pull request's merge state:
// per-resource (repository id + PR number), appended on material change
// only, so the record history is the resource's state timeline rather than
// a polling log.
type PullMergeFact struct {
	Repo         string           `json:"repo"`
	RepositoryID int64            `json:"repository_id"`
	PRNumber     int              `json:"pr_number"`
	State        PullRequestState `json:"state"`
	Merged       bool             `json:"merged"`
	// MergeCommitSHA is set exactly when Merged: on an unmerged PR the
	// forge's merge_commit_sha names a test-merge commit, which is not a
	// fact about the PR and is unrepresentable here.
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	// BaseRef is the PR's base at observation; completion compares it to
	// the binding's admitted base.
	BaseRef string `json:"base_ref"`
	// HeadSHA is the PR's head at observation; completion compares it to
	// the binding's recorded head, so a merge of code the daemon never
	// admitted (a post-publication push, update-branch merge, or
	// force-push) never completes the unit.
	HeadSHA    string    `json:"head_sha"`
	ObservedAt time.Time `json:"observed_at"`
}

// Validate reports whether the fact is well-formed and internally
// consistent: merged state and its commit come and go together, and a
// merged PR cannot claim to be open.
func (f PullMergeFact) Validate() error {
	if f.Repo == "" {
		return fmt.Errorf("pull merge fact repo: %w", ErrEmptyField)
	}
	if f.RepositoryID <= 0 {
		return fmt.Errorf("pull merge fact repository_id %d: %w", f.RepositoryID, ErrNonPositive)
	}
	if f.PRNumber <= 0 {
		return fmt.Errorf("pull merge fact pr_number %d: %w", f.PRNumber, ErrNonPositive)
	}
	if !f.State.valid() {
		return fmt.Errorf("pull merge fact %s#%d state %q: %w",
			f.Repo, f.PRNumber, f.State, ErrInvalidPullRequestState)
	}
	if f.Merged != (f.MergeCommitSHA != "") {
		return fmt.Errorf("pull merge fact %s#%d merged=%v with merge_commit_sha %q: %w",
			f.Repo, f.PRNumber, f.Merged, f.MergeCommitSHA, ErrMergeFactInconsistent)
	}
	if f.Merged && f.State != PullRequestClosed {
		return fmt.Errorf("pull merge fact %s#%d merged while %s: %w",
			f.Repo, f.PRNumber, f.State, ErrMergeFactInconsistent)
	}
	if f.BaseRef == "" {
		return fmt.Errorf("pull merge fact %s#%d base_ref: %w", f.Repo, f.PRNumber, ErrEmptyField)
	}
	if f.HeadSHA == "" {
		return fmt.Errorf("pull merge fact %s#%d head_sha: %w", f.Repo, f.PRNumber, ErrEmptyField)
	}
	if f.ObservedAt.IsZero() {
		return fmt.Errorf("pull merge fact %s#%d observed_at: %w", f.Repo, f.PRNumber, ErrMissingTimestamp)
	}
	if f.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("pull merge fact %s#%d observed_at: %w", f.Repo, f.PRNumber, ErrTimestampNotUTC)
	}
	return nil
}

// MaterialChangeFrom reports whether this observation differs from the
// previous recorded fact in anything but its instant: the append-on-material-
// change rule's single definition, so the store method stays mechanical.
func (f PullMergeFact) MaterialChangeFrom(prev PullMergeFact) bool {
	f.ObservedAt = prev.ObservedAt
	return f != prev
}

// IssueStateFact is one observation of a bound issue's lifecycle state,
// appended on material change like PullMergeFact.
type IssueStateFact struct {
	Repo         string     `json:"repo"`
	RepositoryID int64      `json:"repository_id"`
	IssueNumber  int        `json:"issue_number"`
	State        IssueState `json:"state"`
	// ClosedByCommitSHA is the commit id of the issue's latest `closed`
	// event, when the forge attributes the closure to a commit; empty for
	// an open issue or a closure with no commit attribution (a manual
	// close). It is the explicit closed-by link §5.18's issue criterion
	// evaluates — "closed while some PR happened to be merged" is exactly
	// the inference the capture vocabulary refuses to represent.
	ClosedByCommitSHA string    `json:"closed_by_commit_sha,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
}

// Validate reports whether the fact is well-formed and internally
// consistent: only a closed issue carries a closing commit.
func (f IssueStateFact) Validate() error {
	if f.Repo == "" {
		return fmt.Errorf("issue state fact repo: %w", ErrEmptyField)
	}
	if f.RepositoryID <= 0 {
		return fmt.Errorf("issue state fact repository_id %d: %w", f.RepositoryID, ErrNonPositive)
	}
	if f.IssueNumber <= 0 {
		return fmt.Errorf("issue state fact issue_number %d: %w", f.IssueNumber, ErrNonPositive)
	}
	if !f.State.valid() {
		return fmt.Errorf("issue state fact %s#%d state %q: %w",
			f.Repo, f.IssueNumber, f.State, ErrInvalidIssueState)
	}
	if f.ClosedByCommitSHA != "" && f.State != IssueClosed {
		return fmt.Errorf("issue state fact %s#%d closed_by_commit_sha on %s issue: %w",
			f.Repo, f.IssueNumber, f.State, ErrIssueFactInconsistent)
	}
	if f.ObservedAt.IsZero() {
		return fmt.Errorf("issue state fact %s#%d observed_at: %w", f.Repo, f.IssueNumber, ErrMissingTimestamp)
	}
	if f.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("issue state fact %s#%d observed_at: %w", f.Repo, f.IssueNumber, ErrTimestampNotUTC)
	}
	return nil
}

// MaterialChangeFrom reports whether this observation differs from the
// previous recorded fact in anything but its instant.
func (f IssueStateFact) MaterialChangeFrom(prev IssueStateFact) bool {
	f.ObservedAt = prev.ObservedAt
	return f != prev
}

// WorkUnitCompletion is the write-once record that the unit's declared
// criterion was exactly satisfied: the §5.18 "unit done" fact. It is only
// ever constructed by EvaluateWorkUnitCompletion, so its coordinates always
// restate the observation that satisfied the criterion.
type WorkUnitCompletion struct {
	UnitID    WorkUnitID              `json:"unit_id"`
	Criterion CompletionCriterionKind `json:"criterion"`
	PRNumber  int                     `json:"pr_number"`
	// MergeCommitSHA is the merge that satisfied the criterion.
	MergeCommitSHA string `json:"merge_commit_sha"`
	// BoundIssue restates the closed issue for issue-closure criteria and
	// is absent otherwise (the criterion's contract, revalidated here).
	BoundIssue *int      `json:"bound_issue,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Validate reports whether the completion is well-formed: the criterion is
// registered and its bound-issue contract holds both ways, so a decoded row
// cannot claim an issue-closure completion without naming the issue, or a
// PR-merge completion that smuggles one in.
func (c WorkUnitCompletion) Validate() error {
	if c.UnitID == "" {
		return fmt.Errorf("work unit completion unit_id: %w", ErrEmptyID)
	}
	if !c.Criterion.valid() {
		return fmt.Errorf("work unit completion %s criterion %q: %w",
			c.UnitID, c.Criterion, ErrInvalidCompletionCriterion)
	}
	if c.PRNumber <= 0 {
		return fmt.Errorf("work unit completion %s pr_number %d: %w", c.UnitID, c.PRNumber, ErrNonPositive)
	}
	if c.MergeCommitSHA == "" {
		return fmt.Errorf("work unit completion %s merge_commit_sha: %w", c.UnitID, ErrEmptyField)
	}
	if c.Criterion.requiresBoundIssue() != (c.BoundIssue != nil) {
		return fmt.Errorf("work unit completion %s criterion %s with bound_issue %v: %w",
			c.UnitID, c.Criterion, c.BoundIssue != nil, ErrCompletionInconsistent)
	}
	if c.BoundIssue != nil && *c.BoundIssue <= 0 {
		return fmt.Errorf("work unit completion %s bound_issue %d: %w", c.UnitID, *c.BoundIssue, ErrNonPositive)
	}
	if c.RecordedAt.IsZero() {
		return fmt.Errorf("work unit completion %s recorded_at: %w", c.UnitID, ErrMissingTimestamp)
	}
	if c.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("work unit completion %s recorded_at: %w", c.UnitID, ErrTimestampNotUTC)
	}
	return nil
}

// Equal reports value equality (the bound-issue pointer by value, instants
// by time.Equal), so the store's reconstruction re-gate can require a
// stored completion to be exactly one the trusted evaluator re-derives.
func (c WorkUnitCompletion) Equal(o WorkUnitCompletion) bool {
	if c.UnitID != o.UnitID || c.Criterion != o.Criterion ||
		c.PRNumber != o.PRNumber || c.MergeCommitSHA != o.MergeCommitSHA ||
		!c.RecordedAt.Equal(o.RecordedAt) {
		return false
	}
	if (c.BoundIssue == nil) != (o.BoundIssue == nil) {
		return false
	}
	return c.BoundIssue == nil || *c.BoundIssue == *o.BoundIssue
}

// EvaluateWorkUnitCompletion is the single trusted site for §5.18's exact
// completion rule. It derives a completion from the recorded declaration,
// the daemon-recorded PR binding, and the observed facts, or reports false
// when they do not satisfy the declared criterion:
//
//   - a fact about any other repository or PR never matches (a related
//     merge);
//   - an unmerged or merely closed PR never completes;
//   - a merge into any base but the binding's admitted base never completes
//     (a stacked or retargeted PR);
//   - a merge whose head is not the binding's recorded head never completes
//     (a post-publication push, update-branch merge, or force-push merged
//     code the daemon never admitted);
//   - the issue-closure criterion additionally requires the bound issue,
//     in the binding's repository, closed by exactly the observed merge
//     commit (a partial merge, a manual closure, or a closure by any other
//     commit never completes).
//
// issue may be nil when the criterion does not evaluate issue state. The
// completion's instant is the satisfying observation's, so replays of the
// same facts converge byte-identically on the write-once record.
func EvaluateWorkUnitCompletion(
	decl WorkUnitDeclaration,
	binding WorkUnitPRBinding,
	pull PullMergeFact,
	issue *IssueStateFact,
) (WorkUnitCompletion, bool) {
	if binding.UnitID != decl.ID {
		return WorkUnitCompletion{}, false
	}
	if pull.RepositoryID != binding.RepositoryID || pull.PRNumber != binding.PRNumber {
		return WorkUnitCompletion{}, false
	}
	if !pull.Merged || pull.MergeCommitSHA == "" {
		return WorkUnitCompletion{}, false
	}
	if pull.BaseRef != binding.BaseRef {
		return WorkUnitCompletion{}, false
	}
	if pull.HeadSHA != binding.HeadSHA {
		return WorkUnitCompletion{}, false
	}
	completion := WorkUnitCompletion{
		UnitID: decl.ID, Criterion: decl.CompletionCriterion,
		PRNumber: binding.PRNumber, MergeCommitSHA: pull.MergeCommitSHA,
		RecordedAt: pull.ObservedAt,
	}
	switch decl.CompletionCriterion {
	case CompletionBoundPRMerged:
	case CompletionBoundIssueClosedByMergedPR:
		if decl.BoundIssue == nil || issue == nil {
			return WorkUnitCompletion{}, false
		}
		if issue.RepositoryID != binding.RepositoryID || issue.IssueNumber != *decl.BoundIssue {
			return WorkUnitCompletion{}, false
		}
		if issue.State != IssueClosed || issue.ClosedByCommitSHA != pull.MergeCommitSHA {
			return WorkUnitCompletion{}, false
		}
		completion.BoundIssue = decl.BoundIssue
		completion.RecordedAt = laterOf(pull.ObservedAt, issue.ObservedAt)
	}
	if err := completion.Validate(); err != nil {
		return WorkUnitCompletion{}, false
	}
	return completion, true
}

// laterOf picks the later of two instants: a completion is recorded at the
// moment its last satisfying observation landed.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
