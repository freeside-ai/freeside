package store

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const insertInstanceSettingsRevisionSQL = `
INSERT INTO instance_settings_revisions (revision, section, set_by, set_at, body)
VALUES (?, ?, ?, ?, ?)`

// AppendInstanceSettingsRevision records the next instance settings
// revision, which the store numbers here, after the instance's last. The
// domain constructor refuses a value its section's rules refuse and a clear
// of a required section. It is on InternalTx because the instance settings
// are daemon-internal and never synchronized to clients.
func (tx *InternalTx) AppendInstanceSettingsRevision(
	ctx context.Context, in domain.InstanceSettingsInput, setAt time.Time,
) (domain.InstanceSettingsRevision, error) {
	var last int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM instance_settings_revisions`).Scan(&last); err != nil {
		return domain.InstanceSettingsRevision{}, fmt.Errorf("append instance settings revision: %w", err)
	}
	revision, err := domain.NewInstanceSettingsRevision(in, last+1, setAt)
	if err != nil {
		return domain.InstanceSettingsRevision{}, fmt.Errorf("append instance settings revision: %w", err)
	}
	body, err := encode(revision)
	if err != nil {
		return domain.InstanceSettingsRevision{}, fmt.Errorf("append instance settings revision: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertInstanceSettingsRevisionSQL, revision.Revision, revision.Section,
		revision.SetBy, formatTime(revision.SetAt), body); err != nil {
		return domain.InstanceSettingsRevision{}, fmt.Errorf("append instance settings revision: %w", err)
	}
	return revision, nil
}

// InstanceSettingsRevisions returns every instance settings revision in
// revision order. A row that fails its decode, or whose columns disagree with
// its body, fails the read.
//
// The rows are not a grant: they hold authority-bearing values written under
// the policy of their day. The run settings loader in daemoncompose is the
// gate that re-checks them against current policy before a run uses them.
func (tx *ReadTx) InstanceSettingsRevisions(ctx context.Context) ([]domain.InstanceSettingsRevision, error) {
	revisions, err := queryAll(ctx, tx.tx, scanInstanceSettingsRevision,
		`SELECT revision, section, body FROM instance_settings_revisions ORDER BY revision`)
	if err != nil {
		return nil, fmt.Errorf("instance settings revisions: %w", err)
	}
	return revisions, nil
}

// InstanceSettingsRecord folds the instance settings revisions into the
// current value of each section; none set is an empty record at revision 0.
// Like InstanceSettingsRevisions, it is not a grant.
func (tx *ReadTx) InstanceSettingsRecord(ctx context.Context) (domain.InstanceSettingsRecord, error) {
	revisions, err := tx.InstanceSettingsRevisions(ctx)
	if err != nil {
		return domain.InstanceSettingsRecord{}, err
	}
	record, err := domain.NewInstanceSettingsRecord(revisions)
	if err != nil {
		return domain.InstanceSettingsRecord{}, fmt.Errorf("instance settings record: %w", err)
	}
	return record, nil
}

func scanInstanceSettingsRevision(row scanner) (domain.InstanceSettingsRevision, error) {
	var (
		revision int
		section  string
		body     []byte
	)
	if err := row.Scan(&revision, &section, &body); err != nil {
		return domain.InstanceSettingsRevision{}, err
	}
	r, err := decode[domain.InstanceSettingsRevision](body)
	if err != nil {
		return domain.InstanceSettingsRevision{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if r.Revision != revision || string(r.Section) != section {
		return domain.InstanceSettingsRevision{}, errRowInconsistent
	}
	return r, nil
}
