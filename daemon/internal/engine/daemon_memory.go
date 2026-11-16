package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// DaemonMemory reserves memory for the containers the daemon starts on its
// own, such as an image upkeep build, a ward proof pass, or a model discovery
// probe (plan §5.4). No run asks for this work, so it waits in the memory
// order ahead of every task launch (store.RequireMachineCapacity), and its
// reservation holds until the caller proves every container it started gone.
// Share one value per daemon. A nil *DaemonMemory reserves nothing, for
// compositions and tests that bound no memory.
type DaemonMemory struct {
	Store *store.Store
	// Bound is the bound the instance started with. A budget the operator
	// records later replaces its BudgetMiB at the next reservation
	// (launchBoundAt), as for a launch.
	Bound  ward.LaunchBound
	Now    func() time.Time
	Logger *slog.Logger
	// Poll is how often a waiting reservation asks the gate again. Memory
	// frees only when another reservation's holder releases it, which
	// signals nothing here. Zero means daemonMemoryPoll.
	Poll time.Duration

	mu sync.Mutex
	// held is each reservation work of this process was granted and has not
	// yet released, so a sweep for what an earlier pass left (ListLeft)
	// never proves a live pass's containers gone before it starts them.
	held map[domain.InvocationID]bool
}

// daemonMemoryPoll is short beside store.LaunchWaitRefresh, so a waiting
// reservation refreshes its wait well inside the lease, and long enough that
// a waiting pass costs the store little.
const daemonMemoryPoll = 10 * time.Second

// DaemonWork names one piece of the daemon's own work and what it reserves:
// at most VMs containers of Size at once.
type DaemonWork struct {
	Kind domain.LaunchReservationKind
	ID   domain.InvocationID
	Size ward.ContainerSize
	VMs  int
}

// NewDaemonWorkID returns a reservation id for one piece of the daemon's own
// work: the prefix names the work, and a random suffix keeps two instances
// on one host, which share one container runtime, apart.
func NewDaemonWorkID(prefix string) (domain.InvocationID, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("daemon work id: %w", err)
	}
	return domain.InvocationID(prefix + "-" + hex.EncodeToString(suffix[:])), nil
}

// DaemonReservation is a granted reservation, or the absence of one when no
// budget applies. Its zero value holds nothing.
type DaemonReservation struct {
	memory *DaemonMemory
	id     domain.InvocationID
}

// Context returns ctx carrying the reservation, for the launch guard that
// checks a container start runs under one (ward.WithLaunchReservation).
// Without a reservation it returns ctx unchanged.
func (r DaemonReservation) Context(ctx context.Context) context.Context {
	if r.id == "" {
		return ctx
	}
	return ward.WithLaunchReservation(ctx, r.id)
}

// Held reports whether a reservation was written, which needs a budget.
func (r DaemonReservation) Held() bool { return r.id != "" }

// ID is the reservation's id, empty when none was written.
func (r DaemonReservation) ID() domain.InvocationID { return r.id }

// Release ends the reservation once proof shows every container the work
// started is gone; proof is the caller's error from that check, nil when it
// passed. A failed proof keeps the reservation, logged, because an unproven
// container may still hold its memory, and releasing would let the budget
// overcommit; a start or a later pass of the same work releases it once
// proved. It runs detached from ctx, so work canceled at shutdown still
// releases what it proved. Releasing nothing does nothing.
func (r DaemonReservation) Release(ctx context.Context, proof error) error {
	if r.id == "" {
		return nil
	}
	// The work is done either way; a reservation it keeps is left for a
	// sweep to prove.
	r.memory.unmarkHeld(r.id)
	ctx = context.WithoutCancel(ctx)
	if proof != nil {
		r.memory.logger().WarnContext(ctx, "daemon memory reservation kept: its containers are not proved gone",
			"reservation", r.id, "error", proof)
		return nil
	}
	return r.memory.Store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ReleaseDaemonReservation(ctx, r.id, r.memory.now())
	})
}

// Wait reserves work's memory, waiting in the memory order until the gate
// grants it or ctx ends. A reservation that can never fit the budget, or a
// runtime with no recorded overhead, is refused at once and never waits.
// While it waits it refreshes its wait at least every
// store.LaunchWaitRefresh, so the wait lapses with the lease once the waiter
// is gone. A caller that holds what another reservation's release needs,
// such as the engine's pass, the publication lane, or another daemon
// reservation, must use TryReserve instead, or the daemon could wait on
// itself.
func (m *DaemonMemory) Wait(ctx context.Context, work DaemonWork) (DaemonReservation, error) {
	return m.reserve(ctx, work, true)
}

// TryReserve reserves work's memory if the gate grants it now, and otherwise
// refuses with a *domain.MachineCapacityError (domain.ErrMachineCapacity)
// without joining the memory order.
func (m *DaemonMemory) TryReserve(ctx context.Context, work DaemonWork) (DaemonReservation, error) {
	return m.reserve(ctx, work, false)
}

func (m *DaemonMemory) reserve(ctx context.Context, work DaemonWork, wait bool) (DaemonReservation, error) {
	if m == nil {
		return DaemonReservation{}, nil
	}
	// Read before the gate's write, so unbudgeted work writes nothing. No
	// command clears a recorded budget, so a budget read here still applies
	// in the gate, which reads it again in its own transaction.
	bound, err := currentLaunchBound(ctx, m.Store, m.Bound)
	if err != nil {
		return DaemonReservation{}, err
	}
	reservation, ok, err := bound.DaemonReservation(work.ID, work.Kind, work.Size, work.VMs)
	if err != nil || !ok {
		return DaemonReservation{}, err
	}
	poll := m.Poll
	if poll == 0 {
		poll = daemonMemoryPoll
	}
	var (
		ticker    *time.Ticker
		refreshed time.Time
	)
	// Held before the gate's write commits a grant, so a sweep that reads
	// the granted row before this returns never proves the work's containers
	// gone before it starts them. A refusal unmarks it unless it was held
	// already, as a re-reserved open id is.
	wasHeld := m.markHeld(work.ID)
	// Once a wait is recorded, every return but a grant removes it, so a
	// waiter that gives up for any reason holds back no launch. A grant
	// removes it in the gate.
	fail := func(err error) (DaemonReservation, error) {
		if !wasHeld {
			m.unmarkHeld(work.ID)
		}
		if refreshed.IsZero() {
			return DaemonReservation{}, err
		}
		return DaemonReservation{}, errors.Join(err, m.leave(ctx, work.ID))
	}
	for {
		now := m.now()
		err := m.Store.Write(ctx, func(tx *store.WriteTx) error {
			gate, err := launchBoundAt(ctx, &tx.ReadTx, m.Bound)
			if err != nil {
				return err
			}
			return tx.RequireMachineCapacity(ctx, gate.BudgetMiB, reservation, now)
		})
		if err == nil {
			return DaemonReservation{memory: m, id: work.ID}, nil
		}
		if !wait || !errors.Is(err, domain.ErrMachineCapacity) {
			return fail(err)
		}
		if refreshed.IsZero() || now.Sub(refreshed) >= store.LaunchWaitRefresh {
			if writeErr := m.Store.Write(ctx, func(tx *store.WriteTx) error {
				return tx.RecordDaemonLaunchWait(ctx, work.ID, now)
			}); writeErr != nil {
				// fail leaves only a wait this waiter recorded; this write may
				// have recorded one before it failed.
				refreshed = now
				return fail(writeErr)
			}
			if refreshed.IsZero() {
				m.logger().InfoContext(ctx, "daemon work waits for memory",
					"reservation", work.ID, "kind", work.Kind, "error", err)
			}
			refreshed = now
		}
		if ticker == nil {
			ticker = time.NewTicker(poll)
			defer ticker.Stop()
		}
		select {
		case <-ctx.Done():
			return fail(context.Cause(ctx))
		case <-ticker.C:
		}
	}
}

// leave removes a wait its waiter gives up, so it holds back no launch.
func (m *DaemonMemory) leave(ctx context.Context, id domain.InvocationID) error {
	ctx = context.WithoutCancel(ctx)
	return m.Store.Write(ctx, func(tx *store.WriteTx) error { return tx.LeaveLaunchWait(ctx, id) })
}

// ListLeft returns the daemon's own reservations that are open and that no
// work of this process holds: what an earlier process, or a pass of this one
// that could not prove its containers gone, left. A start or a later pass
// releases each once it proves that work's containers gone.
func (m *DaemonMemory) ListLeft(ctx context.Context) ([]domain.LaunchReservation, error) {
	if m == nil {
		return nil, nil
	}
	var open []domain.LaunchReservation
	if err := m.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		open, err = tx.ListOpenDaemonReservations(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var left []domain.LaunchReservation
	for _, r := range open {
		if !m.held[r.InvocationID] {
			left = append(left, r)
		}
	}
	return left, nil
}

// markHeld marks id held and reports whether it was held already.
func (m *DaemonMemory) markHeld(id domain.InvocationID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held == nil {
		m.held = make(map[domain.InvocationID]bool)
	}
	was := m.held[id]
	m.held[id] = true
	return was
}

func (m *DaemonMemory) unmarkHeld(id domain.InvocationID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, id)
}

// Recovered returns the handle of a reservation an earlier process left
// open, so the start that proves its containers gone can release it.
func (m *DaemonMemory) Recovered(id domain.InvocationID) DaemonReservation {
	if m == nil {
		return DaemonReservation{}
	}
	return DaemonReservation{memory: m, id: id}
}

// ClearWaits removes the waits an earlier process left. A start calls it
// before any site reserves.
func (m *DaemonMemory) ClearWaits(ctx context.Context) error {
	if m == nil {
		return nil
	}
	return m.Store.Write(ctx, func(tx *store.WriteTx) error { return tx.ClearDaemonLaunchWaits(ctx) })
}

func (m *DaemonMemory) now() time.Time {
	if m.Now == nil {
		return time.Now().UTC()
	}
	return m.Now().UTC()
}

func (m *DaemonMemory) logger() *slog.Logger {
	if m.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Logger
}
