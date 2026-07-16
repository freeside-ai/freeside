package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// ErrResultHeadMismatch marks a fixture/reviewer fault: a committed review
// result ran against a head other than the one its invocation id requested.
// It is distinct from exec.ErrStaleHead (freshness vs the current expected
// head): this is the request-binding violation, checked first (§5.3's one
// committed intent per id; issue #36). A holder of the delivered result never
// silently substitutes a different head for the committed request.
var ErrResultHeadMismatch = errors.New("fake: review result head does not match the requested head")

// ReviewScript is one scripted review scenario, keyed to an invocation id
// via Script. Progression is call-step-counted like StageScript: execution
// lag is PendingInspects (Inspect observes StatusRunning), delivery lag is
// PendingPolls (Poll observes exec.ErrResultNotReady); the two are
// independent, as they are for a real forge reviewer.
type ReviewScript struct {
	// PendingInspects is how many Inspect calls observe StatusRunning
	// before the review reads as completed.
	PendingInspects int `json:"pending_inspects"`
	// PendingPolls is how many Poll calls return exec.ErrResultNotReady
	// before the result commits ("delayed review"); it applies only to a
	// delivering outcome (complete), never to a failed or gone review.
	PendingPolls int `json:"pending_polls"`
	// Outcome is how the review ends once execution lag is spent; it reuses
	// the stage Outcome vocabulary and must be set explicitly (the zero value
	// is invalid and fails loud, like any unknown outcome). OutcomeComplete
	// delivers a bare Result; OutcomeFail and OutcomeCrashBeforeResult commit no
	// result (Poll returns exec.ErrNoResult); OutcomeCrashAfterResult commits
	// the result and then loses the session (StatusGone, the result still
	// pollable by id, §5.3).
	Outcome Outcome `json:"outcome"`
	// Result carries the review's head and findings; the fake stamps
	// InvocationID. A stale-head scenario scripts a Result.HeadSHA that
	// differs from the head the test expects, so Verify fails.
	Result exec.ReviewResult `json:"result"`
}

// reviewSession is the transient per-invocation progress: the provider
// session a restart loses.
type reviewSession struct {
	script   ReviewScript
	inspects int
	polls    int
	lost     bool // crash observed; session state is gone
	finished bool // execution reached a terminal, non-crash outcome
}

// ReviewSource is the permanent scripted fake of exec.ReviewSource. Like
// StageDriver, NewReviewSourceAt makes the durable facets survive a real
// restart while the sessions map stays transient.
type ReviewSource struct {
	mu        sync.Mutex
	dir       string // persistence dir; "" means in-memory only
	scripts   map[domain.InvocationID]ReviewScript
	sessions  map[domain.InvocationID]*reviewSession
	committed map[domain.InvocationID]exec.ReviewResult
	intents   map[domain.InvocationID]exec.ReviewRequest
}

// NewReviewSource returns an empty in-memory fake; register scenarios with
// Script before RequestReview. Use NewReviewSourceAt for restart-recovery
// fixtures.
func NewReviewSource() *ReviewSource {
	return &ReviewSource{
		scripts:   make(map[domain.InvocationID]ReviewScript),
		sessions:  make(map[domain.InvocationID]*reviewSession),
		committed: make(map[domain.InvocationID]exec.ReviewResult),
		intents:   make(map[domain.InvocationID]exec.ReviewRequest),
	}
}

// NewReviewSourceAt returns a fake backed by dir, loading any state left by a
// prior instance (load-or-create). Reconstruction is the restart boundary,
// exactly as NewStageDriverAt: durable facets reload, no live session does, so
// every intent without a committed result reads as a lost session
// (StatusGone, ErrNoResult) while a committed result stays pollable by id.
func NewReviewSourceAt(dir string) (*ReviewSource, error) {
	st, err := loadReviewState(dir)
	if err != nil {
		return nil, err
	}
	s := &ReviewSource{
		dir:       dir,
		scripts:   st.Scripts,
		sessions:  make(map[domain.InvocationID]*reviewSession),
		committed: st.Committed,
		intents:   st.Intents,
	}
	for id := range s.intents {
		s.sessions[id] = &reviewSession{script: s.scripts[id], lost: true}
	}
	return s, nil
}

// persistLocked writes the durable facets to disk, excluding transient
// session progress. A no-op for an in-memory fake (empty dir). Callers hold
// s.mu.
func (s *ReviewSource) persistLocked() error {
	if s.dir == "" {
		return nil
	}
	return atomicWrite(s.dir, reviewStateFile, reviewState{
		Scripts:   s.scripts,
		Committed: s.committed,
		Intents:   s.intents,
	})
}

// mustPersistLocked persists or panics; see StageDriver.mustPersistLocked for
// why a persistence failure is fatal and atomic rather than a returned error.
// Callers hold s.mu.
func (s *ReviewSource) mustPersistLocked(op string, id domain.InvocationID) {
	if err := s.persistLocked(); err != nil {
		panic(fmt.Errorf("fake review source %s %s: %w", op, id, err))
	}
}

// Script registers the scenario for an invocation id.
func (s *ReviewSource) Script(id domain.InvocationID, sc ReviewScript) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Clone the scripted result so a later mutation of the caller's input
	// slice cannot reach the registered scenario (issue #35).
	sc.Result = cloneReviewResult(sc.Result)
	s.scripts[id] = sc
	s.mustPersistLocked("script", id)
}

// RequestReview commits the review intent. A second request with the same id
// returns exec.ErrDuplicateStart; an unscripted id returns ErrUnscripted.
func (s *ReviewSource) RequestReview(_ context.Context, id domain.InvocationID, req exec.ReviewRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; ok {
		return fmt.Errorf("fake review source request %s: %w", id, exec.ErrDuplicateStart)
	}
	if _, ok := s.committed[id]; ok {
		return fmt.Errorf("fake review source request %s: %w", id, exec.ErrDuplicateStart)
	}
	script, ok := s.scripts[id]
	if !ok {
		return fmt.Errorf("fake review source request %s: %w", id, ErrUnscripted)
	}
	s.sessions[id] = &reviewSession{
		script:   script,
		inspects: script.PendingInspects,
		polls:    script.PendingPolls,
	}
	// Record the committed intent durably (one per id): a restart reconciles
	// a requested id by Poll/Verify, never by requesting again. This committed
	// copy is what Verify binds the result's head to (#36). exec.ReviewRequest
	// is scalar-only (RunID, HeadSHA), so the map assignment is a full
	// value-copy snapshot: immutable regardless of the caller and stable
	// across restart (persist.go round-trips it). A future reference-typed
	// field on ReviewRequest would need a defensive copy here, as cloneReviewResult
	// does for results.
	s.intents[id] = req
	s.mustPersistLocked("request", id)
	return nil
}

// Inspect consumes one execution step: StatusRunning while scripted inspects
// remain, then the outcome's terminal status. Delivery lag is Poll's, not
// Inspect's; complete/fail commit no result here (Poll delivers), while
// crash-after-result commits the result before the session loss it reports.
func (s *ReviewSource) Inspect(_ context.Context, id domain.InvocationID) (exec.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return "", fmt.Errorf("fake review source inspect %s: %w", id, exec.ErrUnknownInvocation)
	}
	switch {
	case sess.lost:
		return exec.StatusGone, nil
	case sess.finished:
		return finishedReviewStatus(sess.script.Outcome), nil
	case sess.inspects > 0:
		sess.inspects--
		return exec.StatusRunning, nil
	}

	// Execution lag spent: apply the outcome on this observing call.
	switch sess.script.Outcome {
	case OutcomeComplete:
		sess.finished = true
		return exec.StatusCompleted, nil
	case OutcomeFail:
		sess.finished = true
		return exec.StatusFailed, nil
	case OutcomeCrashBeforeResult:
		sess.lost = true
		return exec.StatusGone, nil
	case OutcomeCrashAfterResult:
		sess.lost = true
		s.commit(id, sess.script.Result)
		s.mustPersistLocked("inspect", id)
		return exec.StatusGone, nil
	}
	return "", fmt.Errorf("fake review source inspect %s: unknown outcome %q", id, sess.script.Outcome)
}

// Poll returns the committed review result, identically on every call. Before
// a result commits it returns exec.ErrResultNotReady; a review that ends
// without one (failed, or a session lost before any result) returns
// exec.ErrNoResult.
func (s *ReviewSource) Poll(_ context.Context, id domain.InvocationID) (exec.ReviewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrUnknownInvocation)
	}
	if r, ok := s.committed[id]; ok {
		// Return a clone so a caller mutating the delivered slice cannot
		// alter the committed snapshot (#35).
		return cloneReviewResult(r), nil
	}
	if sess.lost {
		return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrNoResult)
	}
	// No committed result and the session is live: the answer tracks how far
	// Inspect has driven execution, never the eventual outcome, so a review
	// still running reads not-ready, not no-result (Collect's discipline).
	switch sess.script.Outcome {
	case OutcomeComplete:
		if sess.polls > 0 {
			sess.polls--
			return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrResultNotReady)
		}
		r := s.commit(id, sess.script.Result)
		s.mustPersistLocked("poll", id)
		return r, nil
	case OutcomeFail:
		// A failed review yields no result, but only once execution has
		// reached the failure; until Inspect consumes the lag it is running.
		if sess.finished {
			return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrNoResult)
		}
		return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrResultNotReady)
	case OutcomeCrashBeforeResult, OutcomeCrashAfterResult:
		// The crash is observed via Inspect (which sets lost, and commits the
		// crash-after result); until then the review is still running.
		return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrResultNotReady)
	}
	return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: unknown outcome %q", id, sess.script.Outcome)
}

// commit stamps identity onto the scripted result and records it, returning
// the stored value. Callers hold s.mu.
func (s *ReviewSource) commit(id domain.InvocationID, r exec.ReviewResult) exec.ReviewResult {
	r.InvocationID = id
	// Store and return independent snapshots: Poll returns this value
	// directly, so neither the committed copy nor the delivered one may
	// alias the script or each other (#35).
	s.committed[id] = cloneReviewResult(r)
	return cloneReviewResult(r)
}

// Verify gates a committed result in two ordered steps. First the binding
// check: the result's head must match the head committed under this invocation
// id by RequestReview, else ErrResultHeadMismatch (a result that ran against a
// head we never requested can never gate anything, whatever the caller
// expects; #36). Only then the freshness check: the (now request-bound) head
// must match expectedHead, else exec.ErrStaleHead (a correctly requested but
// superseded head still fails after the PR advances, §5.3). Before a result is
// committed it returns exec.ErrResultNotReady (freshness of an undelivered
// review is unknowable), or exec.ErrNoResult when the review will never commit
// one (a failed or lost session): "never" is not "not yet".
func (s *ReviewSource) Verify(_ context.Context, id domain.InvocationID, expectedHead string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("fake review source verify %s: %w", id, exec.ErrUnknownInvocation)
	}
	r, ok := s.committed[id]
	if !ok {
		// Mirror Poll's timing: "never" (the session is lost, or a failed
		// review whose execution has reached the failure) is distinct from
		// "not yet" (still running, or an undelivered complete review).
		if sess.lost || (sess.script.Outcome == OutcomeFail && sess.finished) {
			return fmt.Errorf("fake review source verify %s: %w", id, exec.ErrNoResult)
		}
		return fmt.Errorf("fake review source verify %s: %w", id, exec.ErrResultNotReady)
	}
	// Binding: the result must belong to the head this id committed. Checked
	// before freshness so a result that ran against an unrequested head fails
	// as a binding violation, not as a coincidentally-matching current head.
	if intent := s.intents[id]; r.HeadSHA != intent.HeadSHA {
		return fmt.Errorf("fake review source verify %s: result head %q, requested %q: %w",
			id, r.HeadSHA, intent.HeadSHA, ErrResultHeadMismatch)
	}
	if r.HeadSHA != expectedHead {
		return fmt.Errorf("fake review source verify %s: result head %q, expected %q: %w",
			id, r.HeadSHA, expectedHead, exec.ErrStaleHead)
	}
	return nil
}

// finishedReviewStatus is the terminal execution status a finished review
// reports on repeat Inspects: failed for OutcomeFail, completed otherwise. A
// session only reaches finished through the delivering/failing outcome arms of
// Inspect, so a zero or unknown Outcome never gets here.
func finishedReviewStatus(o Outcome) exec.Status {
	if o == OutcomeFail {
		return exec.StatusFailed
	}
	return exec.StatusCompleted
}
