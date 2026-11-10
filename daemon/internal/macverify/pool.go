package macverify

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrNoFreeSlot refuses a job when every slot is leased or quarantined. The
// pool never waits: queueing belongs to the engine's admission (#2882).
var ErrNoFreeSlot = errors.New("macverify: no free verification slot")

// slotPool leases one slot per command, held across its pre-job probe and
// the command itself. A quarantined slot, one whose absence
// proof failed, is never leased again by this daemon process; after a
// restart, slot-run's prepare refuses it until it owns no process.
type slotPool struct {
	mu          sync.Mutex
	slots       []string
	leased      map[string]bool
	quarantined map[string]bool
}

func newSlotPool(slots []string) *slotPool {
	return &slotPool{
		slots:       slices.Clone(slots),
		leased:      make(map[string]bool, len(slots)),
		quarantined: make(map[string]bool, len(slots)),
	}
}

func (p *slotPool) lease() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, slot := range p.slots {
		if !p.leased[slot] && !p.quarantined[slot] {
			p.leased[slot] = true
			return slot, nil
		}
	}
	return "", fmt.Errorf("%w: %d slots, %d quarantined", ErrNoFreeSlot, len(p.slots), len(p.quarantined))
}

// leaseNamed leases exactly slot, refusing one that is leased, quarantined,
// or not in the pool. The conformance pass proves each slot by name and
// never waits for one.
func (p *slotPool) leaseNamed(slot string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case !slices.Contains(p.slots, slot):
		return fmt.Errorf("slot %s is not in the pool", slot)
	case p.quarantined[slot]:
		return fmt.Errorf("slot %s is quarantined", slot)
	case p.leased[slot]:
		return fmt.Errorf("%w: slot %s is leased", ErrNoFreeSlot, slot)
	}
	p.leased[slot] = true
	return nil
}

func (p *slotPool) release(slot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.leased, slot)
}

func (p *slotPool) quarantine(slot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.leased, slot)
	p.quarantined[slot] = true
}
