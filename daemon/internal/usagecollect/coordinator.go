package usagecollect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Defaults for Config fields left zero. The daemon sets RefreshAge to twice
// the doctor interval: a reading is stale once one scheduled pass is missed.
const (
	DefaultRefreshAge  = 48 * time.Hour
	DefaultBackoffBase = 5 * time.Minute
	DefaultBackoffCap  = 6 * time.Hour
)

var (
	// ErrNoCollector means no collector is registered for the harness client
	// of the pool's collector enrollment, so nothing can be collected.
	ErrNoCollector = errors.New("no usage collector for the collector enrollment's harness client")
	// ErrNotRunning means the coordinator's Run has not started or has
	// returned, so no collection can start.
	ErrNotRunning = errors.New("usage collection is not running")

	errCollectorChanged = errors.New("the pool's collector changed during collection")
	errDiscarded        = errors.New("reading discarded")
	errUnchanged        = errors.New("collector unchanged")
)

// Outcome is how one collection ended. The zero value is invalid by design.
type Outcome string

const (
	// OutcomeCollected means the collector answered, supported or
	// unsupported, and the record holds the answer.
	OutcomeCollected Outcome = "collected"
	// OutcomeFailed means the attempt failed; the record keeps its readings
	// and holds the failure as its refresh status.
	OutcomeFailed Outcome = "failed"
	// OutcomeDiscarded means the answer was taken under a collector or
	// generation since replaced, so nothing was written.
	OutcomeDiscarded Outcome = "discarded"
	// OutcomeBackingOff means an earlier failure's backoff had not ended, so
	// the collector was not asked.
	OutcomeBackingOff Outcome = "backing_off"
)

// AllOutcomes is the single registration point for outcomes.
var AllOutcomes = []Outcome{OutcomeCollected, OutcomeFailed, OutcomeDiscarded, OutcomeBackingOff}

func (o Outcome) valid() bool {
	switch o {
	case OutcomeCollected, OutcomeFailed, OutcomeDiscarded, OutcomeBackingOff:
		return true
	default:
		return false
	}
}

// Report is a pool's record after a refresh, with each window's freshness.
// Observation is nil when the pool has no record yet.
type Report struct {
	Pool         domain.UsagePoolID           `json:"pool"`
	Outcome      Outcome                      `json:"outcome"`
	BackoffUntil *time.Time                   `json:"backoff_until"`
	Observation  *domain.PoolUsageObservation `json:"observation"`
	Windows      []WindowFreshness            `json:"windows"`
}

// Config tunes a Coordinator. A zero field takes its default.
type Config struct {
	RefreshAge  time.Duration
	BackoffBase time.Duration
	BackoffCap  time.Duration
	Now         func() time.Time
	Logger      *slog.Logger
}

// Coordinator runs collection for every pool. At most one collection per pool
// is in flight; a trigger that arrives while one runs joins it. Backoff and
// reset timers are in memory: a restart starts with no backoff and rearms
// the timers from the stored records.
type Coordinator struct {
	store      *store.Store
	collectors map[domain.HarnessClientKind]ward.UsageCollector
	cfg        Config

	mu       sync.Mutex
	runCtx   context.Context
	inflight map[domain.UsagePoolID]*collection
	backoffs map[domain.UsagePoolID]backoff
	resets   map[domain.UsagePoolID]*time.Timer
	// epochs counts each pool's reassignments, so a collection that settles
	// after one leaves the new collector's backoff and timer alone.
	epochs  map[domain.UsagePoolID]int
	running sync.WaitGroup
}

type collection struct {
	done   chan struct{}
	report Report
	err    error
	// again asks for another collection after this one; guarded by c.mu.
	again bool
	// againPastBackoff makes that next collection start even when this one
	// fails into backoff; guarded by c.mu. Only CaptureArrived sets it.
	againPastBackoff bool
	// epoch is the pool's reassignment count when this collection started.
	epoch int
}

type backoff struct {
	failures int
	until    time.Time
}

// New returns a coordinator over st that collects through collectors, one per
// harness client. Nothing collects until Start.
func New(
	st *store.Store, collectors map[domain.HarnessClientKind]ward.UsageCollector, cfg Config,
) *Coordinator {
	if cfg.RefreshAge <= 0 {
		cfg.RefreshAge = DefaultRefreshAge
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = DefaultBackoffBase
	}
	if cfg.BackoffCap <= 0 {
		cfg.BackoffCap = DefaultBackoffCap
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Coordinator{
		store: st, collectors: collectors, cfg: cfg,
		inflight: map[domain.UsagePoolID]*collection{},
		backoffs: map[domain.UsagePoolID]backoff{},
		resets:   map[domain.UsagePoolID]*time.Timer{},
		epochs:   map[domain.UsagePoolID]int{},
	}
}

// Start serves triggers until ctx ends. It arms a reset timer for every
// stored record, and starts a collection now for a pool whose window reset
// passed after its reading, as while the daemon was down. It returns once
// triggers are served, so a caller starts it before anything can trigger.
func (c *Coordinator) Start(ctx context.Context) {
	c.mu.Lock()
	c.runCtx = ctx
	c.mu.Unlock()
	var records []domain.PoolUsageObservation
	if err := c.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		records, err = tx.ListPoolUsageObservations(ctx)
		return err
	}); err != nil {
		c.cfg.Logger.Warn("usage collection: arm reset timers", "error", err)
	}
	now := c.cfg.Now()
	c.mu.Lock()
	for _, record := range records {
		c.armReset(record, false)
	}
	c.mu.Unlock()
	for _, record := range records {
		if resetMissed(record, now) {
			c.trigger(record.Pool, "reset passed while stopped", false)
		}
	}
}

// Wait returns after Start's ctx ends, the reset timers stop, and every
// collection in flight finishes.
func (c *Coordinator) Wait() {
	c.mu.Lock()
	ctx := c.runCtx
	c.mu.Unlock()
	<-ctx.Done()
	c.mu.Lock()
	for pool, timer := range c.resets {
		timer.Stop()
		delete(c.resets, pool)
	}
	c.mu.Unlock()
	c.running.Wait()
}

// CollectAll starts one collection for every pool that has an enrollment, the
// scheduled doctor pass's trigger, and returns without waiting for them.
func (c *Coordinator) CollectAll(ctx context.Context) error {
	var pools []domain.UsagePoolID
	if err := c.store.Read(ctx, func(tx *store.ReadTx) error {
		identities, err := tx.ListAuthIdentities(ctx)
		if err != nil {
			return err
		}
		seen := map[domain.UsagePoolID]bool{}
		for _, identity := range identities {
			pool := identity.EffectiveUsagePool()
			if seen[pool] {
				continue
			}
			seen[pool] = true
			switch _, err := tx.DefaultPoolCollector(ctx, pool); {
			case err == nil:
				pools = append(pools, pool)
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("list usage pools: %w", err)
	}
	for _, pool := range pools {
		c.trigger(pool, "scheduled pass", false)
	}
	return nil
}

// RequestRefresh starts a collection that begins after the request, and
// returns without waiting for it: the client's refresh, after its command
// commits. A collection in flight gets another after it. During backoff it
// starts nothing, as Refresh does not ask the collector then.
func (c *Coordinator) RequestRefresh(pool domain.UsagePoolID) {
	c.trigger(pool, "operator refresh", true)
}

// RateLimitEvent starts a collection for a pool whose run reported a provider
// rate-limit event, and returns without waiting for it.
func (c *Coordinator) RateLimitEvent(pool domain.UsagePoolID) {
	c.trigger(pool, "rate-limit event", false)
}

// CaptureArrived starts a collection for a pool whose collector now holds a
// new answer in memory: the writer turn's usage capture that a Claude
// collector keeps (ward.ClaudeUsageCollector). It does nothing unless
// enrollment is the pool's collector at its current generation, since only
// that capture can reach the record. It returns without waiting for the
// collection.
//
// Unlike every other trigger it starts the collection during backoff, and a
// collection in flight gets another after it even if that one fails into
// backoff. Backoff spares a provider repeated requests; asking a collector
// that answers from memory costs the provider nothing, and the backoff an
// empty memory built up after a restart would otherwise hold the first
// capture for hours. A collector that calls the provider must not be fed by
// this trigger.
func (c *Coordinator) CaptureArrived(
	ctx context.Context, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int,
) error {
	var collects bool
	if err := c.store.Read(ctx, func(tx *store.ReadTx) error {
		record, err := storedOrDefault(ctx, tx, pool)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil || record.Collector != enrollment {
			return err
		}
		current, err := currentGeneration(ctx, tx, enrollment)
		collects = current == generation
		return err
	}); err != nil {
		return fmt.Errorf("usage capture for %s: %w", pool, err)
	}
	if !collects {
		return nil
	}
	c.mu.Lock()
	delete(c.backoffs, pool)
	if running := c.inflight[pool]; running != nil {
		running.againPastBackoff = true
	}
	c.mu.Unlock()
	c.trigger(pool, "usage capture", true)
	return nil
}

// Refresh joins the pool's collection in flight, or starts one, and returns
// the record it left. During backoff it returns the stored record and when
// the backoff ends, without asking the collector.
func (c *Coordinator) Refresh(ctx context.Context, pool domain.UsagePoolID) (Report, error) {
	running, until, err := c.start(pool, false)
	if err != nil {
		return Report{}, err
	}
	if running == nil {
		record, err := c.storedRecord(ctx, pool)
		if err != nil {
			return Report{}, err
		}
		report := c.report(pool, OutcomeBackingOff, record)
		report.BackoffUntil = &until
		return report, nil
	}
	select {
	case <-running.done:
		return running.report, running.err
	case <-ctx.Done():
		return Report{}, ctx.Err()
	}
}

// ApplyPartial records one window reading a run's live event carried, leaving
// every other window, and reports whether it was recorded. A reading from
// anyone but the collector, or before the collector's last attempt
// succeeded, is dropped and a full collection starts instead. A reading taken
// under a replaced generation, or older than the one held, is discarded.
func (c *Coordinator) ApplyPartial(
	ctx context.Context, pool domain.UsagePoolID, reading domain.WindowReading,
) (bool, error) {
	var applied domain.PoolUsageObservation
	needsCollection := false
	err := c.store.Write(ctx, func(tx *store.WriteTx) error {
		stored, err := tx.GetPoolUsageObservation(ctx, pool)
		if errors.Is(err, store.ErrNotFound) {
			needsCollection = true
			return errDiscarded
		}
		if err != nil {
			return err
		}
		var ok bool
		if applied, ok = applyPartial(stored, reading); !ok {
			needsCollection = true
			return errDiscarded
		}
		err = tx.RecordPoolUsageObservation(ctx, applied, c.cfg.Now())
		if errors.Is(err, store.ErrReadingGenerationReplaced) || errors.Is(err, store.ErrStaleWrite) {
			return errDiscarded
		}
		return err
	})
	if needsCollection {
		c.trigger(pool, "dropped partial update", false)
	}
	if errors.Is(err, errDiscarded) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("apply partial usage update to %s: %w", pool, err)
	}
	c.mu.Lock()
	c.armReset(applied, false)
	c.mu.Unlock()
	return true, nil
}

// SetCollector reassigns the pool's collector, then starts a collection under
// the new one. The old collector's backoff and reset timer go with it.
func (c *Coordinator) SetCollector(
	ctx context.Context, pool domain.UsagePoolID, collector domain.ClientEnrollmentID,
) (Reassignment, error) {
	result, err := Reassign(ctx, c.store, pool, collector, c.cfg.Now)
	if err != nil || !result.Recorded {
		return result, err
	}
	c.CollectorReassigned(pool)
	return result, nil
}

// CollectorReassigned drops the old collector's backoff and reset timer and
// starts a collection under the new one, without waiting for it. Call it
// once a recorded reassignment has committed, never before: a collection
// that started earlier would read the old collector.
func (c *Coordinator) CollectorReassigned(pool domain.UsagePoolID) {
	c.mu.Lock()
	c.epochs[pool]++
	delete(c.backoffs, pool)
	if timer := c.resets[pool]; timer != nil {
		timer.Stop()
		delete(c.resets, pool)
	}
	c.mu.Unlock()
	c.trigger(pool, "collector reassigned", true)
}

// Reassignment is the outcome of a collector reassignment. Recorded is false
// when the pool already had that collector, so no revision was written.
type Reassignment struct {
	Pool              domain.UsagePoolID        `json:"pool"`
	PreviousCollector domain.ClientEnrollmentID `json:"previous_collector"`
	Collector         domain.ClientEnrollmentID `json:"collector"`
	Recorded          bool                      `json:"recorded"`
}

// Reassign records collector as the pool's collector, stamped by now inside
// the write, as a new record revision, without starting a collection: the
// operator's explicit action and the only way the collector changes. An
// enrollment not on the pool fails with store.ErrEnrollmentNotOnPool.
func Reassign(
	ctx context.Context, st *store.Store, pool domain.UsagePoolID, collector domain.ClientEnrollmentID,
	now func() time.Time,
) (Reassignment, error) {
	var (
		result   Reassignment
		applyErr error
	)
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		// Stamped inside the transaction, so no writer that committed first
		// holds a later instant.
		if result, applyErr = ReassignTx(ctx, tx, pool, collector, now()); applyErr != nil {
			return applyErr
		}
		if !result.Recorded {
			// Nothing written: roll back so the revision does not move.
			return errUnchanged
		}
		return nil
	})
	switch {
	case errors.Is(err, errUnchanged):
		return result, nil
	case applyErr != nil:
		return Reassignment{}, applyErr
	case err != nil:
		return Reassignment{}, fmt.Errorf("reassign the collector of %s to %s: %w", pool, collector, err)
	}
	return result, nil
}

// ReassignTx is Reassign in the caller's transaction, so the record commits
// with whatever else the caller writes. It writes nothing when the pool
// already has collector, and refuses an enrollment not on the pool with
// store.ErrEnrollmentNotOnPool whatever the stored record's stamp.
func ReassignTx(
	ctx context.Context, tx *store.WriteTx, pool domain.UsagePoolID, collector domain.ClientEnrollmentID, at time.Time,
) (Reassignment, error) {
	result := Reassignment{Pool: pool, Collector: collector}
	record, err := storedOrDefault(ctx, &tx.ReadTx, pool)
	if err == nil {
		result.PreviousCollector = record.Collector
		if record.Collector == collector {
			return result, nil
		}
		// Membership first: the write checks revision order before it
		// reaches its own membership check, so a newer stored record would
		// report a stale write in place of the real refusal.
		err = tx.RequireEnrollmentOnPool(ctx, pool, collector)
	}
	if err == nil {
		err = tx.RecordPoolUsageObservation(ctx, reassigned(record, collector), at)
	}
	if err != nil {
		return Reassignment{}, fmt.Errorf("reassign the collector of %s to %s: %w", pool, collector, err)
	}
	result.Recorded = true
	return result, nil
}

// trigger starts a collection without waiting for it, logging why none
// could start. A fresh trigger needs a collection that starts after it.
func (c *Coordinator) trigger(pool domain.UsagePoolID, reason string, fresh bool) {
	if _, until, err := c.start(pool, fresh); err != nil {
		c.cfg.Logger.Warn("usage collection not started", "pool", pool, "trigger", reason, "error", err)
	} else if !until.IsZero() {
		c.cfg.Logger.Info("usage collection backing off", "pool", pool, "trigger", reason, "until", until)
	}
}

// start returns the pool's collection in flight or a new one. During backoff
// it returns no collection and when the backoff ends. A fresh start that
// joins a collection in flight has another run after it: that one may have
// read the pool before the event the trigger reports.
func (c *Coordinator) start(pool domain.UsagePoolID, fresh bool) (*collection, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := c.runCtx
	if ctx == nil || ctx.Err() != nil {
		return nil, time.Time{}, ErrNotRunning
	}
	if running := c.inflight[pool]; running != nil {
		running.again = running.again || fresh
		return running, time.Time{}, nil
	}
	if held := c.backoffs[pool]; c.cfg.Now().Before(held.until) {
		return nil, held.until, nil
	}
	running := &collection{done: make(chan struct{}), epoch: c.epochs[pool]}
	c.inflight[pool] = running
	c.running.Add(1)
	go func() {
		defer c.running.Done()
		report, retryAfter, err := c.collect(ctx, pool)
		// A reassignment during the call discards the old collector's answer;
		// the new collector collects next.
		changed := errors.Is(err, errCollectorChanged)
		if changed {
			report, err = c.report(pool, OutcomeDiscarded, nil), nil
		}
		c.mu.Lock()
		delete(c.inflight, pool)
		c.settle(pool, running, report, retryAfter)
		again := running.again
		if running.againPastBackoff {
			delete(c.backoffs, pool)
		}
		c.mu.Unlock()
		running.report, running.err = report, err
		close(running.done)
		switch {
		case changed || again:
			c.trigger(pool, "event during collection", false)
		case err != nil && !errors.Is(err, ErrNoCollector) && ctx.Err() == nil:
			c.cfg.Logger.Warn("usage collection failed", "pool", pool, "error", err)
		}
	}()
	return running, time.Time{}, nil
}

// settle updates the pool's backoff and reset timer after a collection. A
// collection that started before a reassignment settles nothing: its outcome
// is the old collector's. The caller holds c.mu.
func (c *Coordinator) settle(pool domain.UsagePoolID, running *collection, report Report, retryAfter time.Duration) {
	if running.epoch != c.epochs[pool] {
		return
	}
	switch report.Outcome {
	case OutcomeCollected:
		delete(c.backoffs, pool)
	case OutcomeFailed:
		held := c.backoffs[pool]
		held.failures++
		delay := c.cfg.BackoffBase
		for i := 1; i < held.failures && delay < c.cfg.BackoffCap; i++ {
			delay *= 2
		}
		delay = max(min(delay, c.cfg.BackoffCap), retryAfter)
		held.until = c.cfg.Now().Add(delay)
		c.backoffs[pool] = held
	case OutcomeDiscarded, OutcomeBackingOff:
	}
	if report.Observation != nil {
		c.armReset(*report.Observation, report.Outcome != OutcomeCollected)
	}
}

// armReset arms the pool's one reset timer at the record's next reset, so the
// pool collects once after it, and no sooner than a backoff ends. When owed,
// after a collection that did not land, a reset the record has not seen since
// it passed is still owed its collection, so the timer also retries it, no
// sooner than one backoff base: a collector that keeps answering without
// landing is asked at that pace at most. The caller holds c.mu.
func (c *Coordinator) armReset(record domain.PoolUsageObservation, owed bool) {
	if timer := c.resets[record.Pool]; timer != nil {
		timer.Stop()
		delete(c.resets, record.Pool)
	}
	if c.runCtx == nil || c.runCtx.Err() != nil {
		return
	}
	now := c.cfg.Now()
	at, ok := nextReset(record, now)
	if retry := now.Add(c.cfg.BackoffBase); owed && resetMissed(record, now) && (!ok || retry.Before(at)) {
		at, ok = retry, true
	}
	if !ok {
		return
	}
	pool := record.Pool
	if until := c.backoffs[pool].until; until.After(at) {
		at = until
	}
	c.resets[pool] = time.AfterFunc(at.Sub(now), func() { c.resetReached(pool) })
}

// resetReached collects once after a window's reset. During backoff it tries
// again when the backoff ends, so the reset is not lost. That timer replaces
// the pool's reset timer: an earlier reset would wait for the backoff too, and
// the collection at its end re-arms any later reset the record names.
func (c *Coordinator) resetReached(pool domain.UsagePoolID) {
	_, until, err := c.start(pool, true)
	if err != nil || until.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx.Err() != nil {
		return
	}
	if timer := c.resets[pool]; timer != nil {
		timer.Stop()
	}
	c.resets[pool] = time.AfterFunc(until.Sub(c.cfg.Now()), func() { c.resetReached(pool) })
}

// collect runs one collection: read the record and the collector's current
// generation, ask the collector outside any transaction, then reread and
// write in one client-visible transaction. The write discards an answer
// taken under a collector or generation since replaced.
func (c *Coordinator) collect(ctx context.Context, pool domain.UsagePoolID) (Report, time.Duration, error) {
	var (
		record     domain.PoolUsageObservation
		enrollment domain.ClientEnrollment
		generation int
	)
	if err := c.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if record, err = storedOrDefault(ctx, tx, pool); err != nil {
			return err
		}
		if enrollment, err = tx.GetClientEnrollment(ctx, record.Collector); err != nil {
			return err
		}
		generation, err = currentGeneration(ctx, tx, record.Collector)
		return err
	}); err != nil {
		return Report{}, 0, fmt.Errorf("collect usage for %s: %w", pool, err)
	}
	collector := c.collectors[enrollment.HarnessClient]
	if collector == nil {
		return Report{}, 0, fmt.Errorf("collect usage for %s: %s is %s: %w",
			pool, enrollment.ID, enrollment.HarnessClient, ErrNoCollector)
	}
	var (
		result  ward.UsageCollection
		callErr error
	)
	if generation == 0 {
		callErr = fmt.Errorf("collector %s has no enrollment generation", enrollment.ID)
	} else {
		result, callErr = collector.CollectUsage(ctx, ward.UsageCollectionRequest{
			Enrollment: enrollment, Generation: generation,
		})
	}
	if ctx.Err() != nil {
		return Report{}, 0, ctx.Err()
	}
	if callErr == nil {
		callErr = requireStamped(result.Windows, enrollment.ID, generation)
	}
	var retryAfter time.Duration
	var throttle *ward.UsageThrottleError
	if errors.As(callErr, &throttle) {
		retryAfter = throttle.RetryAfter
	}
	// held is the record as the write found it, which a discarded answer
	// leaves in place.
	var written, held domain.PoolUsageObservation
	err := c.store.Write(ctx, func(tx *store.WriteTx) error {
		// Stamped inside the transaction, so no writer that committed first
		// holds a later instant.
		at := c.cfg.Now()
		stored, err := storedOrDefault(ctx, &tx.ReadTx, pool)
		if err != nil {
			return err
		}
		held = stored
		if stored.Collector != record.Collector {
			return errCollectorChanged
		}
		// A reading that landed during the call, such as a partial update, is
		// later than this answer, which would replace it or drop its window.
		if !sameReadings(stored.Windows, record.Windows) {
			return errDiscarded
		}
		// An answer taken under a generation since replaced is discarded
		// whole, readings or not: it describes a credential that is gone.
		if current, err := currentGeneration(ctx, &tx.ReadTx, record.Collector); err != nil {
			return err
		} else if current != generation {
			return errDiscarded
		}
		written = applyCollection(stored, result, callErr, at)
		err = tx.RecordPoolUsageObservation(ctx, written, at)
		switch {
		case errors.Is(err, store.ErrReadingGenerationReplaced), errors.Is(err, store.ErrStaleWrite):
			// A later reading of a window, such as a partial update that
			// committed during the call, supersedes this answer.
			return errDiscarded
		case err != nil && callErr == nil:
			// The answer itself was refused, so the attempt failed: keep the
			// readings and record why.
			callErr = err
			written = applyCollection(stored, ward.UsageCollection{}, err, at)
			return tx.RecordPoolUsageObservation(ctx, written, at)
		}
		return err
	})
	switch {
	case errors.Is(err, errDiscarded):
		return c.report(pool, OutcomeDiscarded, &held), 0, nil
	case err != nil:
		return Report{}, 0, fmt.Errorf("record usage for %s: %w", pool, err)
	}
	outcome := OutcomeCollected
	if written.LastAttempt.Status == domain.CollectionAttemptFailed {
		outcome = OutcomeFailed
	}
	return c.report(pool, outcome, &written), retryAfter, nil
}

// storedRecord returns the pool's record, nil when it has none.
func (c *Coordinator) storedRecord(ctx context.Context, pool domain.UsagePoolID) (*domain.PoolUsageObservation, error) {
	var record *domain.PoolUsageObservation
	err := c.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetPoolUsageObservation(ctx, pool)
		switch {
		case err == nil:
			record = &stored
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return nil
	})
	return record, err
}

func (c *Coordinator) report(pool domain.UsagePoolID, outcome Outcome, record *domain.PoolUsageObservation) Report {
	report := Report{Pool: pool, Outcome: outcome, Observation: record, Windows: []WindowFreshness{}}
	if record != nil {
		report.Windows = windowsFreshness(*record, c.cfg.Now(), c.cfg.RefreshAge)
	}
	return report
}

// storedOrDefault returns the pool's record, or an empty one under the pool's
// default collector when it has none.
func storedOrDefault(ctx context.Context, tx *store.ReadTx, pool domain.UsagePoolID) (domain.PoolUsageObservation, error) {
	record, err := tx.GetPoolUsageObservation(ctx, pool)
	if !errors.Is(err, store.ErrNotFound) {
		return record, err
	}
	collector, err := tx.DefaultPoolCollector(ctx, pool)
	if err != nil {
		return domain.PoolUsageObservation{}, err
	}
	return domain.PoolUsageObservation{Pool: pool, Collector: collector}, nil
}

// currentGeneration returns the enrollment's current generation ordinal, zero
// when it has none.
func currentGeneration(ctx context.Context, tx *store.ReadTx, id domain.ClientEnrollmentID) (int, error) {
	current, err := tx.CurrentEnrollmentGeneration(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	return current.Ordinal, err
}

// sameReadings reports whether two window sets hold the same readings. A new
// reading of a window always carries a later observation time.
func sameReadings(left, right []domain.WindowReading) bool {
	return slices.EqualFunc(left, right, func(l, r domain.WindowReading) bool {
		return l.Window == r.Window && l.EnrollmentID == r.EnrollmentID && l.Generation == r.Generation &&
			l.ObservedAt.Equal(r.ObservedAt)
	})
}

// requireStamped refuses an answer with a reading the collector did not stamp
// with the enrollment and generation it was asked under. The collector's
// stamp is its claim, so a mismatch is a failed attempt, not a reading.
func requireStamped(readings []domain.WindowReading, enrollment domain.ClientEnrollmentID, generation int) error {
	for _, reading := range readings {
		if reading.EnrollmentID != enrollment || reading.Generation != generation {
			return fmt.Errorf("collector returned window %s stamped %s generation %d, asked under %s generation %d",
				reading.Window, reading.EnrollmentID, reading.Generation, enrollment, generation)
		}
	}
	return nil
}
