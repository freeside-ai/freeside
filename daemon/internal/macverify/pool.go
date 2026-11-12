package macverify

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrNoFreeSlot refuses a job when every slot is leased or quarantined. The
// pool never waits: a job refused while a slot is leased retries on a later
// engine pass, and the engine raises a notice on the run when every slot is
// quarantined, since no retry gets one until the daemon restarts (#3004).
var ErrNoFreeSlot = errors.New("macverify: no free verification slot")

// NoFreeSlotError is the refusal lease returns, matching ErrNoFreeSlot. It
// tells a busy pool, whose leased slot frees, from one whose every slot is
// quarantined, which no later lease in this process can serve.
type NoFreeSlotError struct {
	ExecutorRoot string
	Slots        int
	// Quarantined names the quarantined slots, sorted.
	Quarantined []string
}

func (e *NoFreeSlotError) Error() string {
	return fmt.Sprintf("%v: executor %s, %d slots, %d quarantined",
		ErrNoFreeSlot, e.ExecutorRoot, e.Slots, len(e.Quarantined))
}

// Is matches ErrNoFreeSlot, so a caller that only asks whether a slot was
// free need not know the type.
func (e *NoFreeSlotError) Is(target error) bool { return target == ErrNoFreeSlot }

// AllQuarantined reports whether every slot in a non-empty pool is
// quarantined, so no slot frees without a restart.
func (e *NoFreeSlotError) AllQuarantined() bool {
	return e.Slots > 0 && len(e.Quarantined) == e.Slots
}

// slotPool leases one slot per command, held across its pre-job probe and
// the command itself. A quarantined slot, one whose absence
// proof failed, is never leased again by this daemon process; after a
// restart, slot-run's prepare refuses it until it owns no process.
type slotPool struct {
	mu          sync.Mutex
	root        string
	slots       []string
	leased      map[string]bool
	quarantined map[string]bool
}

func newSlotPool(root string, slots []string) *slotPool {
	return &slotPool{
		root:        root,
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
	quarantined := make([]string, 0, len(p.quarantined))
	for slot := range p.quarantined {
		quarantined = append(quarantined, slot)
	}
	slices.Sort(quarantined)
	return "", &NoFreeSlotError{ExecutorRoot: p.root, Slots: len(p.slots), Quarantined: quarantined}
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
