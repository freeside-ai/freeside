package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// Reconciler polls the external resources a publication owns — the
// branch ref and the pull request — one resource at a time with
// conditional requests (plan §5.11): each resource carries its own
// ETag validator, there is no global cursor, and an unchanged resource
// answers 304, which returns the cached observation without touching
// any state.
//
// The cache is in-memory on purpose: ETags are a bandwidth and
// rate-limit optimization, never correctness. After a restart the
// first poll per resource is unconditional and re-establishes the
// validator; convergence correctness comes from deterministic
// identities and check-before-create, not from this cache.
//
// Concurrency: the Reconciler is data-race-safe but not linearizable.
// The mutex guards the cache, not the read–fetch–update sequence, so
// two concurrent polls of one resource can complete out of order; the
// loser caches the older (internally consistent) validator and
// observation pair, and the next conditional poll simply gets a 200
// and re-syncs. Serializing the whole sequence would hold a lock
// across network I/O for a cache correctness never depends on.
// Eviction advances a process-wide epoch so a response that started before
// eviction may still be returned to its caller but cannot refill any cache.
// Intended usage is one poller per resource.
type Reconciler struct {
	forge *forge

	mu             sync.Mutex
	cacheEpoch     uint64
	refs           map[string]refCacheEntry
	pulls          map[string]pullCacheEntry
	issues         map[string]issueCacheEntry
	reviewActivity map[string]reviewActivityCacheEntry
	labelIssues    map[string]labelIssuesCacheEntry
}

type refCacheEntry struct {
	etag string
	obs  RefObservation
}

type pullCacheEntry struct {
	etag string
	obs  PullObservation
}

type issueCacheEntry struct {
	etag string
	obs  IssueObservation
}

// reviewActivityCacheEntry caches the three list sub-resources of one PR's
// review activity independently: each carries its own validator, so an
// unchanged sub-resource answers 304 and its cached items are reused while a
// changed sibling re-reads. An empty per-part etag makes that part's next poll
// unconditional.
type reviewActivityCacheEntry struct {
	reviewsETag   string
	commentsETag  string
	reactionsETag string
	reviews       []PullReview
	comments      []PullReviewComment
	reactions     []PullDescriptionReaction
}

func refKey(repo, branch string) string {
	return repo + "\x00" + branch
}

func pullKey(repo string, number int) string {
	return fmt.Sprintf("%s\x00%d", repo, number)
}

func issueKey(repo string, number int) string {
	return fmt.Sprintf("%s\x00issue\x00%d", repo, number)
}

func reviewActivityKey(repo string, number int) string {
	return fmt.Sprintf("%s\x00review\x00%d", repo, number)
}

// labelIssuesCacheEntry caches the labeled open-issue list for one
// (repo, label) pair alongside its validator, so an unchanged list answers 304
// and the cached issues are reused. An empty etag makes the next poll
// unconditional.
type labelIssuesCacheEntry struct {
	etag   string
	issues []LabelIssue
}

func labelIssuesKey(repo, label string) string {
	return fmt.Sprintf("%s\x00label\x00%s", repo, label)
}

// RefObservation is the reconciled state of one branch ref.
// NotModified reports that the server confirmed the cached observation
// (a 304); the remaining fields then repeat that cached state.
type RefObservation struct {
	Exists      bool
	SHA         string
	NotModified bool
}

// PullObservation is the reconciled state of one pull request; see
// RefObservation for NotModified. It carries every coordinate the
// publication identity binds — head ref, commit, and repository, base
// ref and repository — so an external change to any of them (a human
// retargeting the PR, say) is visible in the observation itself; an
// observation that dropped them would cache the changed resource's new
// validator and then confirm the change as "unchanged" on every later
// 304.
type PullObservation struct {
	Number   int
	State    string
	Title    string
	Body     string
	HeadRef  string
	HeadSHA  string
	HeadRepo string
	BaseRef  string
	BaseRepo string
	// BaseRepoID, Merged, and MergeCommitSHA carry the PR's observed base
	// identity and merge state for the §5.18 capture hooks; MergeCommitSHA
	// is set exactly when Merged, and BaseRepoID is the only coordinate
	// that can expose a repository name re-bound to a different repository
	// (see prState).
	BaseRepoID     int64
	Merged         bool
	MergeCommitSHA string
	NotModified    bool
}

// IssueObservation is the reconciled state of one issue; see
// RefObservation for NotModified. ClosedByCommitSHA is the commit the
// issue's latest `closed` event attributes the closure to, directly or via
// the closing pull request, or "" for an open issue or a manual closure —
// the explicit closed-by link the §5.18 issue criterion evaluates.
type IssueObservation struct {
	Number            int
	State             string
	ClosedByCommitSHA string
	NotModified       bool
}

// PullReviewObservation is the reconciled native review activity of one pull
// request (plan §5.16, §7): its submitted reviews, inline review comments, and
// description reactions, as observed. It is raw transport, not durable
// evidence — the reconciler filters by reviewer login, normalizes, and bounds
// it before recording. NotModified reports that all three sub-resources
// answered 304, so the whole activity is unchanged.
type PullReviewObservation struct {
	Reviews     []PullReview
	Comments    []PullReviewComment
	Reactions   []PullDescriptionReaction
	NotModified bool
}

// LabelIssuesObservation is the reconciled set of open issues carrying one
// initiator label. NotModified reports that the server confirmed the cached
// list (a 304); Issues then repeats that cached list. The list carries every
// currently-labeled open issue, so an occurrence whose issue is absent from it
// has either lost the label or closed — the intake loop distinguishes those by
// observing the issue directly.
type LabelIssuesObservation struct {
	Issues []LabelIssue
	// RepositoryID is the canonical numeric identity the initiator repository
	// name currently resolves to, observed unconditionally each pass. The intake
	// loop fails closed when it does not equal the configured RepositoryID, so a
	// repository name rebound to a different repository cannot redirect intake
	// (§5.18); the name alone can never surface that rebinding.
	RepositoryID int64
	NotModified  bool
}

// NewReconciler wires a Reconciler. baseURL is the GitHub API root
// (real: https://api.github.com; tests: an httptest server).
func NewReconciler(ts TokenSource, client *http.Client, baseURL string) *Reconciler {
	return &Reconciler{
		forge:          newForge(ts, client, baseURL),
		refs:           map[string]refCacheEntry{},
		pulls:          map[string]pullCacheEntry{},
		issues:         map[string]issueCacheEntry{},
		reviewActivity: map[string]reviewActivityCacheEntry{},
		labelIssues:    map[string]labelIssuesCacheEntry{},
	}
}

// ReconcileRef observes refs/heads/<branch>, conditionally when a
// prior observation holds a validator. Ref absence is an observation
// (the branch may legitimately not exist yet), not an error.
func (r *Reconciler) ReconcileRef(ctx context.Context, repo, branch string) (RefObservation, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return RefObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if branch == "" {
		return RefObservation{}, errors.New("reconcile: empty branch")
	}
	key := refKey(repo, branch)

	r.mu.Lock()
	entry, cached := r.refs[key]
	epoch := r.cacheEpoch
	r.mu.Unlock()
	etag := ""
	if cached {
		etag = entry.etag
	}

	st, err := r.forge.getRef(ctx, ref, branch, etag)
	if err != nil {
		return RefObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if st.NotModified {
		// Returned-object boundary: a 304 answers the validator we sent;
		// trusting one on an unconditional request would fabricate a
		// "confirmed" observation out of nothing.
		if !cached {
			return RefObservation{}, errors.New("reconcile: 304 for a request that sent no validator")
		}
		// entry is the observation the server just confirmed. Nothing is
		// written: a 304 must not churn state (issue #81 acceptance 3).
		obs := entry.obs
		obs.NotModified = true
		return obs, nil
	}

	obs := RefObservation{Exists: st.Exists, SHA: st.SHA}
	r.mu.Lock()
	if epoch == r.cacheEpoch {
		if st.Exists && st.ETag != "" {
			r.refs[key] = refCacheEntry{etag: st.ETag, obs: obs}
		} else {
			// No validator (absent ref, or a response without an ETag): the
			// next poll is unconditional.
			delete(r.refs, key)
		}
	}
	r.mu.Unlock()
	return obs, nil
}

// ReconcilePull observes one pull request, conditionally when a prior
// observation holds a validator.
func (r *Reconciler) ReconcilePull(ctx context.Context, repo string, number int) (PullObservation, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return PullObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if number <= 0 {
		return PullObservation{}, fmt.Errorf("reconcile: invalid pull number %d", number)
	}
	key := pullKey(repo, number)

	r.mu.Lock()
	entry, cached := r.pulls[key]
	epoch := r.cacheEpoch
	r.mu.Unlock()
	etag := ""
	if cached {
		etag = entry.etag
	}

	read, err := r.forge.getPR(ctx, ref, number, etag)
	if err != nil {
		return PullObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if read.NotModified {
		// See ReconcileRef: an unsolicited 304 is refused, not trusted.
		if !cached {
			return PullObservation{}, errors.New("reconcile: 304 for a request that sent no validator")
		}
		obs := entry.obs
		obs.NotModified = true
		return obs, nil
	}

	obs := PullObservation{
		Number:         read.PR.Number,
		State:          read.PR.State,
		Title:          read.PR.Title,
		Body:           read.PR.Body,
		HeadRef:        read.PR.HeadRef,
		HeadSHA:        read.PR.HeadSHA,
		HeadRepo:       read.PR.HeadRepo,
		BaseRef:        read.PR.BaseRef,
		BaseRepo:       read.PR.BaseRepo,
		BaseRepoID:     read.PR.BaseRepoID,
		Merged:         read.PR.Merged,
		MergeCommitSHA: read.PR.MergeCommitSHA,
	}
	r.mu.Lock()
	if epoch == r.cacheEpoch {
		if read.ETag != "" {
			r.pulls[key] = pullCacheEntry{etag: read.ETag, obs: obs}
		} else {
			delete(r.pulls, key)
		}
	}
	r.mu.Unlock()
	return obs, nil
}

// ReconcileIssue observes one issue, conditionally when a prior
// observation holds a validator. A closed issue's observation includes
// the closing-commit walk, so the returned state is always evaluable
// against the §5.18 issue criterion; a failed walk fails the observation
// rather than reporting a closure with no attribution it actually has.
func (r *Reconciler) ReconcileIssue(ctx context.Context, repo string, number int) (IssueObservation, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return IssueObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if number <= 0 {
		return IssueObservation{}, fmt.Errorf("reconcile: invalid issue number %d", number)
	}
	key := issueKey(repo, number)

	r.mu.Lock()
	entry, cached := r.issues[key]
	epoch := r.cacheEpoch
	r.mu.Unlock()
	etag := ""
	if cached {
		etag = entry.etag
	}

	read, err := r.forge.getIssue(ctx, ref, number, etag)
	if err != nil {
		return IssueObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if read.NotModified {
		// See ReconcileRef: an unsolicited 304 is refused, not trusted.
		if !cached {
			return IssueObservation{}, errors.New("reconcile: 304 for a request that sent no validator")
		}
		obs := entry.obs
		obs.NotModified = true
		return obs, nil
	}

	obs := IssueObservation{Number: read.Issue.Number, State: read.Issue.State}
	if read.Issue.State == "closed" {
		commit, eventNodeID, err := r.forge.issueClosingCommit(ctx, ref, number)
		if err != nil {
			return IssueObservation{}, fmt.Errorf("reconcile: %w", err)
		}
		if commit == "" {
			commit, err = r.forge.issueClosureAttribution(ctx, ref, number, eventNodeID)
			if err != nil {
				return IssueObservation{}, fmt.Errorf("reconcile: %w", err)
			}
		}
		obs.ClosedByCommitSHA = commit
	}
	r.mu.Lock()
	if epoch == r.cacheEpoch {
		if read.ETag != "" && (obs.State != "closed" || obs.ClosedByCommitSHA != "") {
			r.issues[key] = issueCacheEntry{etag: read.ETag, obs: obs}
		} else {
			delete(r.issues, key)
		}
	}
	r.mu.Unlock()
	return obs, nil
}

// ReconcilePullReviewActivity observes a pull request's native review activity
// (plan §5.16, §7): its submitted reviews, inline review comments, and
// description reactions, each conditionally when a prior observation holds a
// validator. The three sub-resources are cached independently, so an unchanged
// one rides its 304 while a changed sibling re-reads; the observation reports
// NotModified only when all three are unchanged. Read-only: this never touches
// publication or readiness state.
func (r *Reconciler) ReconcilePullReviewActivity(ctx context.Context, repo string, number int) (PullReviewObservation, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if number <= 0 {
		return PullReviewObservation{}, fmt.Errorf("reconcile: invalid pull number %d", number)
	}
	key := reviewActivityKey(repo, number)

	r.mu.Lock()
	entry := r.reviewActivity[key]
	epoch := r.cacheEpoch
	r.mu.Unlock()

	reviewsRead, err := r.forge.getPullReviews(ctx, ref, number, entry.reviewsETag)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	commentsRead, err := r.forge.getPullReviewComments(ctx, ref, number, entry.commentsETag)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	reactionsRead, err := r.forge.getIssueReactions(ctx, ref, number, entry.reactionsETag)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile: %w", err)
	}

	reviews, reviewsETag, err := resolveListPart(reviewsRead, entry.reviewsETag, entry.reviews)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile reviews: %w", err)
	}
	comments, commentsETag, err := resolveListPart(commentsRead, entry.commentsETag, entry.comments)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile review comments: %w", err)
	}
	reactions, reactionsETag, err := resolveListPart(reactionsRead, entry.reactionsETag, entry.reactions)
	if err != nil {
		return PullReviewObservation{}, fmt.Errorf("reconcile reactions: %w", err)
	}

	obs := PullReviewObservation{
		Reviews: reviews, Comments: comments, Reactions: reactions,
		NotModified: reviewsRead.NotModified && commentsRead.NotModified && reactionsRead.NotModified,
	}
	r.mu.Lock()
	if epoch == r.cacheEpoch {
		r.reviewActivity[key] = reviewActivityCacheEntry{
			reviewsETag: reviewsETag, commentsETag: commentsETag, reactionsETag: reactionsETag,
			reviews: reviews, comments: comments, reactions: reactions,
		}
	}
	r.mu.Unlock()
	return obs, nil
}

// ReconcileLabelIssues observes the open issues carrying label, conditionally
// when a prior observation holds a validator. The list is a single conditional
// resource: a 304 confirms the whole labeled-open set unchanged and reuses the
// cached list; a 200 replaces it. Read-only — occurrence, admission, and start
// decisions belong to the intake loop that consumes this.
func (r *Reconciler) ReconcileLabelIssues(ctx context.Context, repo, label string) (LabelIssuesObservation, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return LabelIssuesObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	if label == "" {
		return LabelIssuesObservation{}, errors.New("reconcile: empty label")
	}
	key := labelIssuesKey(repo, label)

	r.mu.Lock()
	entry := r.labelIssues[key]
	epoch := r.cacheEpoch
	r.mu.Unlock()

	// Resolve the repository's canonical numeric identity unconditionally each
	// pass, before the (conditional) label scan, so the intake loop can fail
	// closed on a rebound name even when the labeled list itself answers 304.
	repositoryID, err := r.forge.getRepositoryID(ctx, ref)
	if err != nil {
		return LabelIssuesObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	read, err := r.forge.getLabelIssues(ctx, ref, label, entry.etag)
	if err != nil {
		return LabelIssuesObservation{}, fmt.Errorf("reconcile: %w", err)
	}
	issues, etag, err := resolveListPart(read, entry.etag, entry.issues)
	if err != nil {
		return LabelIssuesObservation{}, fmt.Errorf("reconcile label issues: %w", err)
	}
	obs := LabelIssuesObservation{Issues: issues, RepositoryID: repositoryID, NotModified: read.NotModified}
	r.mu.Lock()
	if epoch == r.cacheEpoch {
		if etag != "" {
			r.labelIssues[key] = labelIssuesCacheEntry{etag: etag, issues: issues}
		} else {
			// No validator on the 200: the next poll is unconditional.
			delete(r.labelIssues, key)
		}
	}
	r.mu.Unlock()
	return obs, nil
}

// EvictLabelIssues drops the cached validator and list for one (repo, label)
// pair, so the next ReconcileLabelIssues re-fetches unconditionally instead of
// riding a 304. The intake loop calls this when a durable intake write fails
// after a successful fetch already advanced the ETag, so the observation is
// retried on the next tick rather than suppressed until the labeled set changes.
func (r *Reconciler) EvictLabelIssues(repo, label string) {
	r.mu.Lock()
	r.cacheEpoch++
	delete(r.labelIssues, labelIssuesKey(repo, label))
	r.mu.Unlock()
}

// EvictRef drops the cached validator and observation for one branch ref.
func (r *Reconciler) EvictRef(repo, branch string) {
	r.mu.Lock()
	r.cacheEpoch++
	delete(r.refs, refKey(repo, branch))
	r.mu.Unlock()
}

// EvictPull drops the cached validator and observation for one pull request.
func (r *Reconciler) EvictPull(repo string, number int) {
	r.mu.Lock()
	r.cacheEpoch++
	delete(r.pulls, pullKey(repo, number))
	r.mu.Unlock()
}

// EvictIssue drops the cached validator and observation for one issue.
func (r *Reconciler) EvictIssue(repo string, number int) {
	r.mu.Lock()
	r.cacheEpoch++
	delete(r.issues, issueKey(repo, number))
	r.mu.Unlock()
}

// EvictPullReviewActivity drops the cached validators and lists for one PR's
// review activity, so the next ReconcilePullReviewActivity re-fetches every
// sub-resource unconditionally instead of riding a 304. The active-resource
// reconciler calls this when the durable append of the built observations
// fails: getPull* already advanced the ETags on the successful fetch, so
// without eviction the next tick's conditional GET answers 304/NotModified,
// observe suppresses buildNativeReviewObservations, and the un-persisted rows
// are never retried until external review activity changes (issue #497).
func (r *Reconciler) EvictPullReviewActivity(repo string, number int) {
	r.mu.Lock()
	r.cacheEpoch++
	delete(r.reviewActivity, reviewActivityKey(repo, number))
	r.mu.Unlock()
}

// resolveListPart returns the items and validator for one review sub-resource:
// the fresh list on a 200, or the cached list on a solicited 304. As in
// ReconcileRef, an unsolicited 304 (no validator was sent) is refused rather
// than trusted, so a server cannot fabricate a "confirmed" empty observation.
func resolveListPart[E any](read listRead[E], sentETag string, cached []E) ([]E, string, error) {
	if read.NotModified {
		if sentETag == "" {
			return nil, "", errors.New("304 for a request that sent no validator")
		}
		return cached, sentETag, nil
	}
	return read.Items, read.ETag, nil
}
