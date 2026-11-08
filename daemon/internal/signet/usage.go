package signet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// WithUsageRefreshAge sets how long a window reading counts as fresh after it
// was observed: the projection's stale_at is the earlier of the reading's
// reset and its observation plus this age. Zero, the default, leaves a reading
// with no reset without a stale_at.
func WithUsageRefreshAge(age time.Duration) Option {
	return func(s *Service) { s.usageRefreshAge = age }
}

// UsagePoolSnapshot is one usage pool's usage record with its sync metadata,
// matching api/openapi.yaml. AsOfRevision and EntityVersion are the record's;
// a pool with no record yet carries the snapshot's revision and version 0, and
// each write of the record adds one.
type UsagePoolSnapshot struct {
	AsOfRevision  int64          `json:"as_of_revision"`
	EntityVersion int64          `json:"entity_version"`
	Pool          UsagePoolUsage `json:"pool"`
}

// UsagePoolUsage is what the daemon recorded about one pool (plan §5.4, usage
// observation): its declaration, its collector and refresh status, every
// enrollment on it, and the record's readings and the exhausted marks it holds.
// Nothing here is added across pools, and refusal marks stay out: each
// exhausted mark already names its source.
type UsagePoolUsage struct {
	ID                    domain.UsagePoolID `json:"id"`
	Provider              string             `json:"provider"`
	MaxParallelExecutions int                `json:"max_parallel_executions"`
	Plan                  *string            `json:"plan"`
	// Collector is the record's collector, or the pool's default collector
	// before the first record; nil when the pool has no enrollment.
	Collector    *domain.ClientEnrollmentID `json:"collector"`
	LastAttempt  *domain.CollectionAttempt  `json:"last_attempt"`
	Enrollments  []UsagePoolEnrollment      `json:"enrollments"`
	ModelWindows []domain.ModelWindow       `json:"model_windows"`
	Windows      []UsageWindowReading       `json:"windows"`
	// RecordedExhaustedMarks is every mark the record holds, lapsed ones
	// included, so it changes only with a write. The client lapses each by its
	// own clock, as it does a reading's StaleAt.
	RecordedExhaustedMarks []domain.ExhaustedMark `json:"recorded_exhausted_marks"`
}

// UsagePoolEnrollment is one enrollment whose identity meters the pool, with
// the availability answer the record holds for it, if any.
type UsagePoolEnrollment struct {
	EnrollmentID   domain.ClientEnrollmentID `json:"enrollment_id"`
	AuthIdentityID domain.AuthIdentityID     `json:"auth_identity_id"`
	HarnessClient  domain.HarnessClientKind  `json:"harness_client"`
	Availability   *UsageAvailability        `json:"availability"`
}

// UsageAvailability is an enrollment's recorded availability answer.
type UsageAvailability struct {
	Answer     domain.AvailabilityAnswer `json:"answer"`
	ObservedAt time.Time                 `json:"observed_at"`
}

// UsageWindowReading is domain.WindowReading on the wire: the length in whole
// seconds, and the instant the daemon counts the reading stale.
type UsageWindowReading struct {
	Window        string                    `json:"window"`
	EnrollmentID  domain.ClientEnrollmentID `json:"enrollment_id"`
	Generation    int                       `json:"generation"`
	Status        domain.WindowStatus       `json:"status"`
	UsedPercent   *int                      `json:"used_percent"`
	Quantity      *int64                    `json:"quantity"`
	Unit          *string                   `json:"unit"`
	ResetsAt      *time.Time                `json:"resets_at"`
	LengthSeconds *int64                    `json:"length_seconds"`
	ObservedAt    time.Time                 `json:"observed_at"`
	// StaleAt is the earlier of ResetsAt and ObservedAt plus the refresh age,
	// nil when neither is known. A client compares it with its own clock; the
	// daemon projects an instant because a staleness flag would go stale in a
	// client's cache with no write to refresh it.
	StaleAt *time.Time `json:"stale_at"`
}

// RoutingPolicySnapshot is the current routing policy revision with sync
// metadata, matching api/openapi.yaml. The policy row stores no revision of
// its own, so AsOfRevision is the snapshot's; EntityVersion is the policy
// revision, which every change advances.
type RoutingPolicySnapshot struct {
	AsOfRevision  int64                `json:"as_of_revision"`
	EntityVersion int64                `json:"entity_version"`
	Policy        domain.RoutingPolicy `json:"policy"`
}

// usagePoolSnapshots projects every pool, in pool id order.
func usagePoolSnapshots(
	ctx context.Context, tx *store.ReadTx, state store.ServerState, refreshAge time.Duration,
) ([]UsagePoolSnapshot, error) {
	pools, err := tx.ListUsagePools(ctx)
	if err != nil {
		return nil, fmt.Errorf("usage pools: %w", err)
	}
	records, err := tx.ListPoolUsageObservationSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("usage pools: %w", err)
	}
	recorded := make(map[domain.UsagePoolID]store.Snapshotted[domain.PoolUsageObservation], len(records))
	for _, record := range records {
		if err := validateSnapshot(state, record.Snapshot); err != nil {
			return nil, fmt.Errorf("usage pool %q: %w", record.Value.Pool, err)
		}
		recorded[record.Value.Pool] = record
	}
	enrollments, err := poolEnrollments(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("usage pools: %w", err)
	}
	out := make([]UsagePoolSnapshot, 0, len(pools))
	for _, pool := range pools {
		in := usagePoolInput{pool: pool, enrollments: enrollments[pool.ID]}
		if record, ok := recorded[pool.ID]; ok {
			in.record = &record
		} else {
			collector, err := tx.DefaultPoolCollector(ctx, pool.ID)
			switch {
			case err == nil:
				in.defaultCollector = &collector
			case !errors.Is(err, store.ErrNotFound):
				return nil, fmt.Errorf("usage pool %q: %w", pool.ID, err)
			}
		}
		out = append(out, projectUsagePool(in, state.Revision, refreshAge))
	}
	return out, nil
}

// usagePoolInput is what the store holds for one pool.
type usagePoolInput struct {
	pool domain.UsagePool
	// record is nil before the pool's first record write.
	record *store.Snapshotted[domain.PoolUsageObservation]
	// defaultCollector is read only for a pool with no record; nil when the
	// pool has no enrollment.
	defaultCollector *domain.ClientEnrollmentID
	// enrollments is every enrollment on the pool, in id order.
	enrollments []domain.ClientEnrollment
}

// projectUsagePool projects one pool from its record alone, so two reads at
// one revision agree; revision is the snapshot's, carried by a pool with no
// record.
func projectUsagePool(in usagePoolInput, revision int64, refreshAge time.Duration) UsagePoolSnapshot {
	snapshot := UsagePoolSnapshot{
		AsOfRevision: revision,
		Pool: UsagePoolUsage{
			ID: in.pool.ID, Provider: string(in.pool.Provider), MaxParallelExecutions: in.pool.MaxParallelExecutions,
			Collector:   in.defaultCollector,
			Enrollments: []UsagePoolEnrollment{}, ModelWindows: []domain.ModelWindow{},
			Windows: []UsageWindowReading{}, RecordedExhaustedMarks: []domain.ExhaustedMark{},
		},
	}
	var answers []domain.EnrollmentAvailability
	if record := in.record; record != nil {
		snapshot.AsOfRevision, snapshot.EntityVersion = record.Snapshot.AsOfRevision, record.Snapshot.EntityVersion
		observation := record.Value
		snapshot.Pool.Plan, snapshot.Pool.Collector = observation.Plan, &observation.Collector
		snapshot.Pool.LastAttempt = observation.LastAttempt
		snapshot.Pool.ModelWindows = nonNilSlice(observation.ModelWindows)
		for _, reading := range observation.Windows {
			snapshot.Pool.Windows = append(snapshot.Pool.Windows, usageWindowReading(reading, refreshAge))
		}
		snapshot.Pool.RecordedExhaustedMarks = nonNilSlice(observation.RecordedExhaustedMarks())
		answers = observation.Enrollments
	}
	for _, enrollment := range in.enrollments {
		entry := UsagePoolEnrollment{
			EnrollmentID: enrollment.ID, AuthIdentityID: enrollment.AuthIdentityID,
			HarnessClient: enrollment.HarnessClient,
		}
		for _, answer := range answers {
			if answer.EnrollmentID == enrollment.ID {
				entry.Availability = &UsageAvailability{Answer: answer.Answer, ObservedAt: answer.ObservedAt}
			}
		}
		snapshot.Pool.Enrollments = append(snapshot.Pool.Enrollments, entry)
	}
	return snapshot
}

// poolEnrollments groups every enrollment by the pool its identity meters,
// each group in enrollment id order.
func poolEnrollments(
	ctx context.Context, tx *store.ReadTx,
) (map[domain.UsagePoolID][]domain.ClientEnrollment, error) {
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return nil, err
	}
	byPool := make(map[domain.UsagePoolID][]domain.ClientEnrollment)
	for _, identity := range identities {
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return nil, err
		}
		pool := identity.EffectiveUsagePool()
		byPool[pool] = append(byPool[pool], enrollments...)
	}
	for _, enrollments := range byPool {
		slices.SortFunc(enrollments, func(left, right domain.ClientEnrollment) int {
			return cmp.Compare(left.ID, right.ID)
		})
	}
	return byPool, nil
}

func usageWindowReading(reading domain.WindowReading, refreshAge time.Duration) UsageWindowReading {
	wire := UsageWindowReading{
		Window: reading.Window, EnrollmentID: reading.EnrollmentID, Generation: reading.Generation,
		Status: reading.Status, UsedPercent: reading.UsedPercent, Quantity: reading.Quantity,
		Unit: reading.Unit, ResetsAt: reading.ResetsAt, ObservedAt: reading.ObservedAt,
		StaleAt: reading.ResetsAt,
	}
	if reading.Length != nil {
		wire.LengthSeconds = new(int64(*reading.Length / time.Second))
	}
	if refreshAge > 0 {
		if aged := reading.ObservedAt.Add(refreshAge); wire.StaleAt == nil || aged.Before(*wire.StaleAt) {
			wire.StaleAt = &aged
		}
	}
	return wire
}

// currentRoutingPolicySnapshot projects the current routing policy revision,
// nil when none was ever set.
func currentRoutingPolicySnapshot(
	ctx context.Context, tx *store.ReadTx, state store.ServerState,
) (*RoutingPolicySnapshot, error) {
	policy, err := tx.CurrentRoutingPolicy(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("routing policy: %w", err)
	}
	return &RoutingPolicySnapshot{
		AsOfRevision: state.Revision, EntityVersion: int64(policy.Revision), Policy: policy,
	}, nil
}
