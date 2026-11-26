package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Execution hosts (migration 0150) are the machines that run this daemon's
// cordoned launches (plan §5.7, issue #2162). The record is ordinary state:
// every write builds the host it leaves and runs domain.ExecutionHost.Validate
// before it touches a row, and every read decodes through Validate, so a bad
// row fails the read. A client reads the hosts (GET /execution-hosts), so the
// writes live on WriteTx and move the server revision. Every Write moves it,
// so a caller that observes a condition should open a Write only when the
// state it read differs from what it observed; the changed result of each
// set or clear only reports what the Write did.

const selectExecutionHostSQL = `
SELECT id, name, run_account_name, run_account_daemon_owner, reach_address, reach_host_key, cordoned_count_override
FROM execution_hosts`

// ExecutionHost returns the host with the given id, or ErrNotFound.
func (tx *ReadTx) ExecutionHost(ctx context.Context, id domain.ExecutionHostID) (domain.ExecutionHost, error) {
	host, err := tx.scanExecutionHost(ctx, tx.tx.QueryRowContext(ctx, selectExecutionHostSQL+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExecutionHost{}, fmt.Errorf("execution host %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return domain.ExecutionHost{}, fmt.Errorf("get execution host %q: %w", id, err)
	}
	return host, nil
}

// ListExecutionHosts returns every host, ordered by id.
func (tx *ReadTx) ListExecutionHosts(ctx context.Context) ([]domain.ExecutionHost, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT id FROM execution_hosts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list execution hosts: %w", err)
	}
	var ids []domain.ExecutionHostID
	for rows.Next() {
		var id domain.ExecutionHostID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list execution hosts: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list execution hosts: %w", err)
	}
	hosts := make([]domain.ExecutionHost, 0, len(ids))
	for _, id := range ids {
		host, err := tx.ExecutionHost(ctx, id)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func (tx *ReadTx) scanExecutionHost(ctx context.Context, row *sql.Row) (domain.ExecutionHost, error) {
	var (
		host             domain.ExecutionHost
		accountName      sql.NullString
		daemonOwner      bool
		address, hostKey sql.NullString
		override         sql.NullInt64
	)
	if err := row.Scan(&host.ID, &host.Name, &accountName, &daemonOwner, &address, &hostKey, &override); err != nil {
		return domain.ExecutionHost{}, err
	}
	host.RunAccount = domain.HostRunAccount{DaemonOwner: daemonOwner}
	if accountName.Valid {
		host.RunAccount.Name = &accountName.String
	}
	if address.Valid || hostKey.Valid {
		host.Reach = &domain.HostReach{Address: address.String, HostKey: hostKey.String}
	}
	if override.Valid {
		count := int(override.Int64)
		host.CordonedCountOverride = &count
	}
	conditions, err := tx.executionHostConditions(ctx, host.ID)
	if err != nil {
		return domain.ExecutionHost{}, err
	}
	host.Conditions = conditions
	if err := host.Validate(); err != nil {
		return domain.ExecutionHost{}, err
	}
	return host, nil
}

// executionHostConditions returns the host's condition rows in
// AllExecutionHostConditions order. An unknown condition sorts first, and
// Validate refuses it.
func (tx *ReadTx) executionHostConditions(
	ctx context.Context, id domain.ExecutionHostID,
) ([]domain.HostObservedCondition, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT condition, since FROM execution_host_conditions WHERE host_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("execution host %q conditions: %w", id, err)
	}
	conditions := []domain.HostObservedCondition{}
	for rows.Next() {
		var condition, since string
		if err := rows.Scan(&condition, &since); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("execution host %q conditions: %w", id, err)
		}
		at, err := parseTime(since)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("execution host %q condition %q since: %w", id, condition, err)
		}
		conditions = append(conditions, domain.HostObservedCondition{
			Condition: domain.ExecutionHostCondition(condition), Since: at,
		})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("execution host %q conditions: %w", id, err)
	}
	sortExecutionHostConditions(conditions)
	return conditions, nil
}

func sortExecutionHostConditions(conditions []domain.HostObservedCondition) {
	slices.SortFunc(conditions, func(a, b domain.HostObservedCondition) int {
		return cmp.Compare(
			slices.Index(domain.AllExecutionHostConditions, a.Condition),
			slices.Index(domain.AllExecutionHostConditions, b.Condition))
	})
}

// AddExecutionHost stores a new host with any conditions it carries. It
// refuses a host Validate refuses, and a host whose id or name is taken
// (domain.ErrExecutionHostExists).
func (tx *WriteTx) AddExecutionHost(ctx context.Context, host domain.ExecutionHost) error {
	if err := host.Validate(); err != nil {
		return fmt.Errorf("add execution host: %w", err)
	}
	var taken bool
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM execution_hosts WHERE id = ? OR name = ?)`, host.ID, host.Name).
		Scan(&taken); err != nil {
		return fmt.Errorf("add execution host %q: %w", host.ID, err)
	}
	if taken {
		return fmt.Errorf("add execution host %q named %q: %w", host.ID, host.Name, domain.ErrExecutionHostExists)
	}
	var address, hostKey any
	if host.Reach != nil {
		address, hostKey = host.Reach.Address, host.Reach.HostKey
	}
	if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO execution_hosts (id, name, run_account_name, run_account_daemon_owner, reach_address, reach_host_key, cordoned_count_override)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		host.ID, host.Name, host.RunAccount.Name, host.RunAccount.DaemonOwner, address, hostKey,
		host.CordonedCountOverride); err != nil {
		return fmt.Errorf("add execution host %q: %w", host.ID, err)
	}
	for _, observed := range host.Conditions {
		if err := tx.insertExecutionHostCondition(ctx, host.ID, observed); err != nil {
			return fmt.Errorf("add execution host %q: %w", host.ID, err)
		}
	}
	return nil
}

// RemoveExecutionHost deletes the host and its conditions. It refuses the
// daemon's host (domain.ErrDaemonHostRemoval) and a host that doesn't exist
// (ErrNotFound).
func (tx *WriteTx) RemoveExecutionHost(ctx context.Context, id domain.ExecutionHostID) error {
	if id == domain.DaemonHostID {
		return fmt.Errorf("remove execution host %q: %w", id, domain.ErrDaemonHostRemoval)
	}
	if _, err := tx.tx.ExecContext(ctx, `DELETE FROM execution_host_conditions WHERE host_id = ?`, id); err != nil {
		return fmt.Errorf("remove execution host %q: %w", id, err)
	}
	result, err := tx.tx.ExecContext(ctx, `DELETE FROM execution_hosts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove execution host %q: %w", id, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove execution host %q: %w", id, err)
	}
	if removed == 0 {
		return fmt.Errorf("remove execution host %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetExecutionHostCondition records that the host is in the condition since
// the given time and reports whether that changed anything. A host already in
// the condition keeps its earlier since. It refuses a host that doesn't exist
// (ErrNotFound) and a condition or time the record refuses.
func (tx *WriteTx) SetExecutionHostCondition(
	ctx context.Context, id domain.ExecutionHostID, condition domain.ExecutionHostCondition, since time.Time,
) (bool, error) {
	host, err := tx.ExecutionHost(ctx, id)
	if err != nil {
		return false, fmt.Errorf("set execution host condition: %w", err)
	}
	if slices.ContainsFunc(host.Conditions, func(o domain.HostObservedCondition) bool { return o.Condition == condition }) {
		return false, nil
	}
	observed := domain.HostObservedCondition{Condition: condition, Since: since}
	host.Conditions = append(host.Conditions, observed)
	sortExecutionHostConditions(host.Conditions)
	if err := host.Validate(); err != nil {
		return false, fmt.Errorf("set execution host condition: %w", err)
	}
	if err := tx.insertExecutionHostCondition(ctx, id, observed); err != nil {
		return false, fmt.Errorf("set execution host condition: %w", err)
	}
	return true, nil
}

// ClearExecutionHostCondition records that the host is no longer in the
// condition and reports whether it was. It refuses a host that doesn't exist
// (ErrNotFound) and an unknown condition.
func (tx *WriteTx) ClearExecutionHostCondition(
	ctx context.Context, id domain.ExecutionHostID, condition domain.ExecutionHostCondition,
) (bool, error) {
	if err := condition.Validate(); err != nil {
		return false, fmt.Errorf("clear execution host condition: %w", err)
	}
	if _, err := tx.ExecutionHost(ctx, id); err != nil {
		return false, fmt.Errorf("clear execution host condition: %w", err)
	}
	result, err := tx.tx.ExecContext(ctx,
		`DELETE FROM execution_host_conditions WHERE host_id = ? AND condition = ?`, id, condition)
	if err != nil {
		return false, fmt.Errorf("clear execution host %q condition %q: %w", id, condition, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("clear execution host %q condition %q: %w", id, condition, err)
	}
	return removed > 0, nil
}

// SetExecutionHostCordonedCountOverride sets the operator's override of the
// host's cordoned count, or clears it when override is nil, and reports
// whether that changed anything. It refuses a host that doesn't exist
// (ErrNotFound) and an override below 1.
func (tx *WriteTx) SetExecutionHostCordonedCountOverride(
	ctx context.Context, id domain.ExecutionHostID, override *int,
) (bool, error) {
	host, err := tx.ExecutionHost(ctx, id)
	if err != nil {
		return false, fmt.Errorf("set execution host cordoned count override: %w", err)
	}
	unchanged := (host.CordonedCountOverride == nil) == (override == nil) &&
		(override == nil || *host.CordonedCountOverride == *override)
	host.CordonedCountOverride = override
	if err := host.Validate(); err != nil {
		return false, fmt.Errorf("set execution host cordoned count override: %w", err)
	}
	if unchanged {
		return false, nil
	}
	if _, err := tx.tx.ExecContext(ctx,
		`UPDATE execution_hosts SET cordoned_count_override = ? WHERE id = ?`, override, id); err != nil {
		return false, fmt.Errorf("set execution host %q cordoned count override: %w", id, err)
	}
	return true, nil
}

func (tx *WriteTx) insertExecutionHostCondition(
	ctx context.Context, id domain.ExecutionHostID, observed domain.HostObservedCondition,
) error {
	if _, err := tx.tx.ExecContext(ctx,
		`INSERT INTO execution_host_conditions (host_id, condition, since) VALUES (?, ?, ?)`,
		id, observed.Condition, formatTime(observed.Since)); err != nil {
		return fmt.Errorf("execution host %q condition %q: %w", id, observed.Condition, err)
	}
	return nil
}
