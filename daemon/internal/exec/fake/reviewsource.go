package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// ReviewScript is one scripted review scenario, keyed to an invocation id
// via Script. Progression is call-step-counted like StageScript: execution
// lag is PendingInspects (Inspect observes StatusRunning), delivery lag is
// PendingPolls (Poll observes exec.ErrResultNotReady); the two are
// independent, as they are for a real forge reviewer.
type ReviewScript struct {
	// PendingInspects is how many Inspect calls observe StatusRunning
	// before the review reads as completed.
	PendingInspects int
	// PendingPolls is how many Poll calls return exec.ErrResultNotReady
	// before the result commits ("delayed review").
	PendingPolls int
	// Result carries the review's head and findings; the fake stamps
	// InvocationID. A stale-head scenario scripts a Result.HeadSHA that
	// differs from the head the test expects, so Verify fails.
	Result exec.ReviewResult
}

// reviewSession is the per-invocation progress state.
type reviewSession struct {
	script   ReviewScript
	inspects int
	polls    int
}

// ReviewSource is the permanent scripted fake of exec.ReviewSource.
type ReviewSource struct {
	mu        sync.Mutex
	scripts   map[domain.InvocationID]ReviewScript
	sessions  map[domain.InvocationID]*reviewSession
	committed map[domain.InvocationID]exec.ReviewResult
}

// NewReviewSource returns an empty fake; register scenarios with Script
// before RequestReview.
func NewReviewSource() *ReviewSource {
	return &ReviewSource{
		scripts:   make(map[domain.InvocationID]ReviewScript),
		sessions:  make(map[domain.InvocationID]*reviewSession),
		committed: make(map[domain.InvocationID]exec.ReviewResult),
	}
}

// Script registers the scenario for an invocation id.
func (s *ReviewSource) Script(id domain.InvocationID, sc ReviewScript) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[id] = sc
}

// RequestReview commits the review intent. A second request with the same id
// returns exec.ErrDuplicateStart; an unscripted id returns ErrUnscripted.
func (s *ReviewSource) RequestReview(_ context.Context, id domain.InvocationID, _ exec.ReviewRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; ok {
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
	return nil
}

// Inspect consumes one execution step: StatusRunning while scripted inspects
// remain, StatusCompleted after (delivery lag is Poll's, not Inspect's).
func (s *ReviewSource) Inspect(_ context.Context, id domain.InvocationID) (exec.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return "", fmt.Errorf("fake review source inspect %s: %w", id, exec.ErrUnknownInvocation)
	}
	if sess.inspects > 0 {
		sess.inspects--
		return exec.StatusRunning, nil
	}
	return exec.StatusCompleted, nil
}

// Poll consumes one delivery step; once the scripted lag is spent it commits
// the result and returns it identically on every later call: duplicate polls
// are inherent, acceptance is the caller's job.
func (s *ReviewSource) Poll(_ context.Context, id domain.InvocationID) (exec.ReviewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrUnknownInvocation)
	}
	if r, ok := s.committed[id]; ok {
		return r, nil
	}
	if sess.polls > 0 {
		sess.polls--
		return exec.ReviewResult{}, fmt.Errorf("fake review source poll %s: %w", id, exec.ErrResultNotReady)
	}
	r := sess.script.Result
	r.InvocationID = id
	s.committed[id] = r
	return r, nil
}

// Verify checks the committed result's freshness against expectedHead,
// wrapping exec.ErrStaleHead on mismatch. Before a result is committed it
// returns exec.ErrResultNotReady: freshness of an undelivered review is
// unknowable, not assumed.
func (s *ReviewSource) Verify(_ context.Context, id domain.InvocationID, expectedHead string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return fmt.Errorf("fake review source verify %s: %w", id, exec.ErrUnknownInvocation)
	}
	r, ok := s.committed[id]
	if !ok {
		return fmt.Errorf("fake review source verify %s: %w", id, exec.ErrResultNotReady)
	}
	if r.HeadSHA != expectedHead {
		return fmt.Errorf("fake review source verify %s: result head %q, expected %q: %w",
			id, r.HeadSHA, expectedHead, exec.ErrStaleHead)
	}
	return nil
}
