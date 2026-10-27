package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	tipWatchRepositoryID = int64(424242)
	tipA                 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tipB                 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var testTipWatch = repositoryTipWatch{Repo: "owner/repo", RepositoryID: tipWatchRepositoryID, Branch: "main"}

// tipWatchHarness drives the tip watch with a settable tip and records every
// tip the entry point receives.
type tipWatchHarness struct {
	tip      string
	readErr  error
	callErr  error
	handed   []string
	now      time.Time
	reads    int
	readyHit int
}

func (h *tipWatchHarness) reconciler(st *store.Store) *activeResourceReconciler {
	return &activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			h.readyHit++
			return exactPull("open", false), nil
		},
		tipWatch: testTipWatch,
		observeTip: func(_ context.Context, repo, branch string) (string, error) {
			h.reads++
			if repo != testTipWatch.Repo || branch != testTipWatch.Branch {
				return "", errors.New("tip read for the wrong repository or branch")
			}
			return h.tip, h.readErr
		},
		upkeepTipAdvance: func(_ context.Context, observation domain.RepositoryTipObservation) error {
			h.handed = append(h.handed, observation.TipSHA)
			return h.callErr
		},
		now: func() time.Time { return h.now },
	}
}

func readTipObservation(t *testing.T, st *store.Store) (domain.RepositoryTipObservation, bool) {
	t.Helper()
	var (
		observation domain.RepositoryTipObservation
		found       bool
	)
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		observation, found, err = tx.GetRepositoryTipObservation(context.Background(), tipWatchRepositoryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return observation, found
}

func seedTipObservation(t *testing.T, st *store.Store, tip, upkeepTip string) {
	t.Helper()
	if err := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordRepositoryTipObservation(context.Background(), domain.RepositoryTipObservation{
			RepositoryID: tipWatchRepositoryID, Repository: testTipWatch.Repo, Branch: testTipWatch.Branch,
			TipSHA: tip, ObservedAt: activeResourceTestTime.Add(-time.Hour), UpkeepTipSHA: upkeepTip,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func reconcileTipWatch(t *testing.T, r *activeResourceReconciler) []error {
	t.Helper()
	failures, err := reconcileActiveResource(r, context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return failures
}

func TestActiveResourceTipWatchHandsEachNewTipToUpkeepOnce(t *testing.T) {
	st := schedTestStore(t)
	h := &tipWatchHarness{tip: tipA, now: activeResourceTestTime}
	r := h.reconciler(st)

	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("first pass failures = %v", failures)
	}
	first, found := readTipObservation(t, st)
	want := domain.RepositoryTipObservation{
		RepositoryID: tipWatchRepositoryID, Repository: "owner/repo", Branch: "main",
		TipSHA: tipA, ObservedAt: activeResourceTestTime, UpkeepTipSHA: tipA,
	}
	if !found || first != want {
		t.Fatalf("first observation = %+v (found %v), want %+v", first, found, want)
	}

	h.now = activeResourceTestTime.Add(time.Minute)
	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("same-tip pass failures = %v", failures)
	}
	if again, _ := readTipObservation(t, st); again != first {
		t.Fatalf("same-tip pass rewrote the observation: %+v, want %+v", again, first)
	}

	h.tip = tipB
	h.now = activeResourceTestTime.Add(2 * time.Minute)
	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("new-tip pass failures = %v", failures)
	}
	moved, _ := readTipObservation(t, st)
	if moved.TipSHA != tipB || moved.UpkeepTipSHA != tipB || !moved.ObservedAt.Equal(h.now) {
		t.Fatalf("new-tip observation = %+v", moved)
	}
	if got := strings.Join(h.handed, ","); got != tipA+","+tipB {
		t.Fatalf("handed tips = %v, want each tip once", h.handed)
	}
}

// tipWatchForge serves one branch whose ETag follows its SHA, so a read
// with the current validator gets a 304.
type tipWatchForge struct {
	mu  sync.Mutex
	sha string
}

func (f *tipWatchForge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/repos/owner/repo/git/ref/heads/main" {
		http.NotFound(w, r)
		return
	}
	etag := `"` + f.sha + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"ref": "refs/heads/main", "object": map[string]any{"sha": f.sha},
	}); err != nil {
		panic(err)
	}
}

// The base-advance watch and the tip watch share the publish reconciler's
// ref cache. When the base watch reads a new tip first, the tip watch's read
// is a 304 carrying that newer SHA, and it must still fire.
func TestActiveResourceTipWatchFiresOnSharedCacheNotModified(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	seedTipObservation(t, st, tipA, tipA)
	forge := &tipWatchForge{sha: tipB}
	server := httptest.NewServer(http.HandlerFunc(forge.serveHTTP))
	t.Cleanup(server.Close)
	publishReconciler := publish.NewReconciler(activeResourceCacheTokenSource{}, server.Client(), server.URL)
	readTip := refTipObserver(publishReconciler)

	// The base-advance watch consumes the change first.
	if sha, err := readTip(ctx, "owner/repo", "main"); err != nil || sha != tipB {
		t.Fatalf("base watch read = %q, %v", sha, err)
	}
	if obs, err := publishReconciler.ReconcileRef(ctx, "owner/repo", "main"); err != nil || !obs.NotModified {
		t.Fatalf("second read = %+v, %v; want a 304", obs, err)
	}

	h := &tipWatchHarness{now: activeResourceTestTime}
	r := h.reconciler(st)
	r.observeTip = readTip
	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("pass failures = %v", failures)
	}
	if len(h.handed) != 1 || h.handed[0] != tipB {
		t.Fatalf("handed tips = %v, want [%s]", h.handed, tipB)
	}
	if got, _ := readTipObservation(t, st); got.TipSHA != tipB || got.UpkeepTipSHA != tipB {
		t.Fatalf("observation = %+v", got)
	}
}

func TestActiveResourceTipWatchReadFailureWritesNothing(t *testing.T) {
	st := schedTestStore(t)
	activeReadyItem(t, st)
	h := &tipWatchHarness{readErr: errors.New("token mint unavailable"), now: activeResourceTestTime}
	r := h.reconciler(st)

	failures := reconcileTipWatch(t, r)
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "token mint unavailable") {
		t.Fatalf("failures = %v, want the tip read failure", failures)
	}
	if h.readyHit != 1 {
		t.Fatalf("ready-item reads = %d, want the pass to continue to its ready item", h.readyHit)
	}
	if _, found := readTipObservation(t, st); found || len(h.handed) != 0 {
		t.Fatalf("a failed read wrote an observation (found %v) or called upkeep %v", found, h.handed)
	}

	h.readErr, h.tip = nil, tipA
	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("retry failures = %v", failures)
	}
	if got, found := readTipObservation(t, st); !found || got.UpkeepTipSHA != tipA || len(h.handed) != 1 {
		t.Fatalf("retry observation = %+v (found %v), handed %v", got, found, h.handed)
	}
}

func TestActiveResourceTipWatchUpkeepFailureRetriesNextPass(t *testing.T) {
	st := schedTestStore(t)
	activeReadyItem(t, st)
	seedTipObservation(t, st, tipA, tipA)
	h := &tipWatchHarness{tip: tipB, callErr: errors.New("upkeep unavailable"), now: activeResourceTestTime}
	r := h.reconciler(st)

	failures := reconcileTipWatch(t, r)
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "upkeep unavailable") {
		t.Fatalf("failures = %v, want the upkeep failure", failures)
	}
	if h.readyHit != 1 {
		t.Fatalf("ready-item reads = %d, want the pass to continue to its ready item", h.readyHit)
	}
	failed, _ := readTipObservation(t, st)
	if failed.TipSHA != tipB || failed.UpkeepTipSHA != tipA {
		t.Fatalf("observation after a failed call = %+v, want tip %s still pending", failed, tipB)
	}

	h.callErr = nil
	h.now = activeResourceTestTime.Add(time.Minute)
	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("retry failures = %v", failures)
	}
	handled, _ := readTipObservation(t, st)
	if handled.UpkeepTipSHA != tipB || !handled.ObservedAt.Equal(failed.ObservedAt) {
		t.Fatalf("observation after retry = %+v, want upkeep tip %s and the original observed_at", handled, tipB)
	}
	if strings.Join(h.handed, ",") != tipB+","+tipB {
		t.Fatalf("handed tips = %v, want the failed call and its retry", h.handed)
	}
}

func TestActiveResourceTipWatchDoesNotRefireAfterRestart(t *testing.T) {
	st := schedTestStore(t)
	seedTipObservation(t, st, tipA, tipA)
	h := &tipWatchHarness{tip: tipA, now: activeResourceTestTime}

	// A reconciler built fresh over the store holds no tip state of its own.
	if failures := reconcileTipWatch(t, h.reconciler(st)); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	if len(h.handed) != 0 {
		t.Fatalf("handed tips = %v, want none for an already-handled tip", h.handed)
	}
}

// The tip changing between the call and its recording leaves the newer tip
// pending: only the handled tip is recorded, and only UpkeepTipSHA changes.
func TestActiveResourceTipWatchRecordsOnlyTheHandledTip(t *testing.T) {
	st := schedTestStore(t)
	seedTipObservation(t, st, tipA, "")
	h := &tipWatchHarness{tip: tipA, now: activeResourceTestTime}
	r := h.reconciler(st)
	r.upkeepTipAdvance = func(_ context.Context, observation domain.RepositoryTipObservation) error {
		h.handed = append(h.handed, observation.TipSHA)
		seedTipObservation(t, st, tipB, "")
		return nil
	}

	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	got, _ := readTipObservation(t, st)
	if got.TipSHA != tipB || got.UpkeepTipSHA != tipA {
		t.Fatalf("observation = %+v, want tip %s pending over handled %s", got, tipB, tipA)
	}
}

// Another writer (the upkeep job's own triggers) may record UpkeepTipSHA
// while the entry point runs; the handled-tip write must not regress it.
func TestActiveResourceTipWatchKeepsAnotherWritersUpkeepTip(t *testing.T) {
	st := schedTestStore(t)
	seedTipObservation(t, st, tipA, "")
	h := &tipWatchHarness{tip: tipA, now: activeResourceTestTime}
	r := h.reconciler(st)
	r.upkeepTipAdvance = func(_ context.Context, observation domain.RepositoryTipObservation) error {
		h.handed = append(h.handed, observation.TipSHA)
		seedTipObservation(t, st, tipB, tipB)
		return nil
	}

	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	got, _ := readTipObservation(t, st)
	if got.TipSHA != tipB || got.UpkeepTipSHA != tipB {
		t.Fatalf("observation = %+v, want the other writer's upkeep tip %s kept", got, tipB)
	}
}

// A reconfigured branch at the same SHA rewrites the row to name the new
// branch, but the tip is already handled, so upkeep is not called.
func TestActiveResourceTipWatchRecordsBranchChangeWithoutRefiring(t *testing.T) {
	st := schedTestStore(t)
	if err := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordRepositoryTipObservation(context.Background(), domain.RepositoryTipObservation{
			RepositoryID: tipWatchRepositoryID, Repository: testTipWatch.Repo, Branch: "master",
			TipSHA: tipA, ObservedAt: activeResourceTestTime.Add(-time.Hour), UpkeepTipSHA: tipA,
		})
	}); err != nil {
		t.Fatal(err)
	}
	h := &tipWatchHarness{tip: tipA, now: activeResourceTestTime}

	if failures := reconcileTipWatch(t, h.reconciler(st)); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	got, _ := readTipObservation(t, st)
	if got.Branch != "main" || !got.ObservedAt.Equal(activeResourceTestTime) || got.UpkeepTipSHA != tipA {
		t.Fatalf("observation = %+v, want branch main recorded with upkeep tip kept", got)
	}
	if len(h.handed) != 0 {
		t.Fatalf("handed tips = %v, want none for an already-handled SHA", h.handed)
	}
}

func TestActiveResourceWithoutTipWatchReadsNoTip(t *testing.T) {
	st := schedTestStore(t)
	h := &tipWatchHarness{tip: tipA, now: activeResourceTestTime}
	r := h.reconciler(st)
	r.tipWatch = repositoryTipWatch{}

	if failures := reconcileTipWatch(t, r); len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	if _, found := readTipObservation(t, st); found || h.reads != 0 || len(h.handed) != 0 {
		t.Fatalf("an unset watch read %d tips, wrote %v, handed %v", h.reads, found, h.handed)
	}

	r.tipWatch, r.upkeepTipAdvance = testTipWatch, nil
	if _, err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("a tip watch without an entry point reconciled")
	}
}
