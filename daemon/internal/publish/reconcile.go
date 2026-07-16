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
// Intended usage is one poller per resource.
type Reconciler struct {
	forge *forge

	mu    sync.Mutex
	refs  map[string]refCacheEntry
	pulls map[string]pullCacheEntry
}

type refCacheEntry struct {
	etag string
	obs  RefObservation
}

type pullCacheEntry struct {
	etag string
	obs  PullObservation
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
	Number      int
	State       string
	Title       string
	Body        string
	HeadRef     string
	HeadSHA     string
	HeadRepo    string
	BaseRef     string
	BaseRepo    string
	NotModified bool
}

// NewReconciler wires a Reconciler. baseURL is the GitHub API root
// (real: https://api.github.com; tests: an httptest server).
func NewReconciler(ts TokenSource, client *http.Client, baseURL string) *Reconciler {
	return &Reconciler{
		forge: newForge(ts, client, baseURL),
		refs:  map[string]refCacheEntry{},
		pulls: map[string]pullCacheEntry{},
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
	key := repo + "\x00" + branch

	r.mu.Lock()
	entry, cached := r.refs[key]
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
	if st.Exists && st.ETag != "" {
		r.refs[key] = refCacheEntry{etag: st.ETag, obs: obs}
	} else {
		// No validator (absent ref, or a response without an ETag): the
		// next poll is unconditional.
		delete(r.refs, key)
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
	key := fmt.Sprintf("%s\x00%d", repo, number)

	r.mu.Lock()
	entry, cached := r.pulls[key]
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
		Number:   read.PR.Number,
		State:    read.PR.State,
		Title:    read.PR.Title,
		Body:     read.PR.Body,
		HeadRef:  read.PR.HeadRef,
		HeadSHA:  read.PR.HeadSHA,
		HeadRepo: read.PR.HeadRepo,
		BaseRef:  read.PR.BaseRef,
		BaseRepo: read.PR.BaseRepo,
	}
	r.mu.Lock()
	if read.ETag != "" {
		r.pulls[key] = pullCacheEntry{etag: read.ETag, obs: obs}
	} else {
		delete(r.pulls, key)
	}
	r.mu.Unlock()
	return obs, nil
}
