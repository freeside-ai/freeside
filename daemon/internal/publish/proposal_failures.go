package publish

import (
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// proposalFailures is an approved-proposal dispatcher's in-memory failure
// bookkeeping: how many listings of one proposal's GitHub collection have
// failed in a row, and when its current run of failed passes began. Each
// project's proposals drive on their own goroutine (#3215), so its mutex
// guards the maps and is never held across a call.
type proposalFailures struct {
	mu      sync.Mutex
	listing map[domain.ProposalInstanceID]int
	since   map[domain.ProposalInstanceID]time.Time
}

func newProposalFailures() *proposalFailures {
	return &proposalFailures{
		listing: map[domain.ProposalInstanceID]int{},
		since:   map[domain.ProposalInstanceID]time.Time{},
	}
}

// listingFailed counts one more failed listing for id and returns the count.
func (p *proposalFailures) listingFailed(id domain.ProposalInstanceID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listing[id]++
	return p.listing[id]
}

// listingSettled forgets id's failed listings.
func (p *proposalFailures) listingSettled(id domain.ProposalInstanceID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.listing, id)
}

// failing returns when id's current run of failed passes began; failing is
// false, and the run starts at now, when this is its first failure.
func (p *proposalFailures) failing(id domain.ProposalInstanceID, now time.Time) (since time.Time, failing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	since, failing = p.since[id]
	if !failing {
		p.since[id] = now
	}
	return since, failing
}

// passed ends id's run of failed passes.
func (p *proposalFailures) passed(id domain.ProposalInstanceID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.since, id)
}
