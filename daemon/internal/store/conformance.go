package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrBackendNotConformant is returned when an unattended admission names a
// backend with no current passed conformance record: none was ever recorded,
// the newest one records a failure, or it predates configuration binding. It
// is deliberately distinct from ErrNotFound: the admission was refused,
// which is not the same as having no record to return.
var ErrBackendNotConformant = errors.New(
	"backend has no current passed conformance record")

// ErrConformanceGenerationSupplied is returned when a conformance record
// arrives carrying a nonzero generation: the generation is this store's
// append identity, and accepting a caller-supplied one would let a writer
// forge where its proof sits in the supersession order.
var ErrConformanceGenerationSupplied = errors.New(
	"conformance proof generation is store-assigned; a record must arrive with generation zero")

// The durable backend-conformance record (plan §5.7; issues #327, #320): the
// append-only log of what each backend's completed full conformance passes
// proved, bound to the normalized configuration each pass exercised. The
// newest row per backend is its current declaration and the row id is the
// proof generation, mirroring ward's in-memory generation guard: a newer
// append (any outcome) supersedes the older record, and a failed append
// invalidates the declaration. Rows are daemon-internal and written by the
// suite runner through InternalTx, so they carry no
// entity_version/as_of_revision and never bump the server revision.
const (
	recordBackendConformanceSQL = `
INSERT INTO backend_conformance_records
    (backend, outcome, configuration_digest, capabilities, proved_at)
VALUES (?, ?, ?, ?, ?)`
	// Newest row per backend by id (insertion order), never by the
	// RFC3339Nano proved_at column: trailing zeros are trimmed, so
	// sub-second instants misorder lexicographically.
	latestBackendConformanceSQL = `
SELECT id, outcome, configuration_digest, capabilities, proved_at
FROM backend_conformance_records WHERE backend = ? ORDER BY id DESC LIMIT 1`
)

// RecordBackendConformance appends one completed pass's record and returns
// the store-assigned proof generation. Append-only by design: the log is the
// audit trail of every completed pass, and a repeated outcome (a second
// failure after a failure) is a real recorded pass, not a conflict. The
// record must arrive unpersisted (Generation zero) and bound to a concrete
// configuration: the generation is this store's append identity, and
// accepting a caller-supplied one would let a writer forge where its proof
// sits in the supersession order. Migrated unbound rows are audit history,
// never valid new appends.
func (tx *InternalTx) RecordBackendConformance(
	ctx context.Context, record domain.BackendConformance,
) (uint64, error) {
	if err := record.Validate(); err != nil {
		return 0, fmt.Errorf("record backend conformance: %w", err)
	}
	if record.Generation != 0 {
		return 0, fmt.Errorf("record backend conformance for %q: generation %d: %w",
			record.Backend, record.Generation, ErrConformanceGenerationSupplied)
	}
	if !record.ConfigurationBound() {
		return 0, fmt.Errorf("record backend conformance for %q: %w",
			record.Backend, domain.ErrConformanceConfigurationUnbound)
	}
	capabilities, err := json.Marshal(record.Capabilities)
	if err != nil {
		return 0, fmt.Errorf("record backend conformance for %q: %w", record.Backend, err)
	}
	result, err := tx.tx.ExecContext(ctx, recordBackendConformanceSQL,
		string(record.Backend), string(record.Outcome), string(record.ConfigurationDigest),
		string(capabilities),
		formatTime(record.ProvedAt))
	if err != nil {
		return 0, fmt.Errorf("record backend conformance for %q: %w", record.Backend, err)
	}
	generation, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record backend conformance for %q: %w", record.Backend, err)
	}
	if generation <= 0 {
		return 0, fmt.Errorf("record backend conformance for %q: assigned generation %d: %w",
			record.Backend, generation, domain.ErrNonPositive)
	}
	return uint64(generation), nil
}

// LatestBackendConformance reconstructs a backend's current declaration: the
// newest appended record, with presence reported separately because an empty
// log is the legitimate "never proved anything" state, not an error (the
// LookupExecutionAdmission shape). Gets validate after reading: the decoded
// record re-runs domain validation, including the class capability ceiling,
// so a tampered row claiming beyond what the class's suite could ever prove
// fails closed instead of reconstructing as a wider declaration.
func (tx *ReadTx) LatestBackendConformance(
	ctx context.Context, backend domain.RunnerBackendClass,
) (domain.BackendConformance, bool, error) {
	var (
		id                  int64
		outcome             string
		configurationDigest string
		capabilities        string
		provedAt            string
	)
	err := tx.tx.QueryRowContext(ctx, latestBackendConformanceSQL, string(backend)).
		Scan(&id, &outcome, &configurationDigest, &capabilities, &provedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.BackendConformance{}, false, nil
	case err != nil:
		return domain.BackendConformance{}, false,
			fmt.Errorf("latest backend conformance for %q: %w", backend, err)
	}
	if id <= 0 {
		return domain.BackendConformance{}, false,
			fmt.Errorf("latest backend conformance for %q: row id %d: %w",
				backend, id, domain.ErrNonPositive)
	}
	var caps domain.CapabilitySnapshot
	if err := json.Unmarshal([]byte(capabilities), &caps); err != nil {
		return domain.BackendConformance{}, false,
			fmt.Errorf("latest backend conformance for %q capabilities: %w", backend, err)
	}
	at, err := parseTime(provedAt)
	if err != nil {
		return domain.BackendConformance{}, false,
			fmt.Errorf("latest backend conformance for %q proved_at %q: %w", backend, provedAt, err)
	}
	record := domain.BackendConformance{
		Backend:             backend,
		Outcome:             domain.ConformanceOutcome(outcome),
		ConfigurationDigest: domain.Digest(configurationDigest),
		Capabilities:        caps,
		Generation:          uint64(id),
		ProvedAt:            at,
	}
	if err := record.Validate(); err != nil {
		return domain.BackendConformance{}, false,
			fmt.Errorf("latest backend conformance for %q: %w", backend, err)
	}
	return record, true, nil
}

// RequireBackendConformant is the backend-conformance half of §5.7's
// unattended conditions, checked in the admitting transaction (issue #320):
// the named backend's current durable declaration must exist, record a pass,
// and cover the admission's capability snapshot. Like
// RequireUnattendedAdmissible it is a recording-time precondition on new
// unattended operation, not part of a record's meaning, so reconstruction
// (scanExecutionAdmission) deliberately does not re-run it: a lapsed or
// superseded conformance must stop what happens next, not make recorded
// history unreadable. attended_dev is exempt by the owner-ratified reading
// of #320: §5.7 admits a weaker, unproven runner class there on purpose.
func (tx *ReadTx) RequireBackendConformant(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	if admission.OperatingMode != domain.ModeUnattended {
		return nil
	}
	record, found, err := tx.LatestBackendConformance(
		ctx, domain.RunnerBackendClass(admission.Backend))
	if err != nil {
		return fmt.Errorf("admission %q: %w", admission.InvocationID, err)
	}
	if !found {
		return fmt.Errorf("admission %q backend %q: %w",
			admission.InvocationID, admission.Backend, ErrBackendNotConformant)
	}
	if record.Outcome != domain.ConformancePassed {
		return fmt.Errorf("admission %q backend %q conformance generation %d records %q: %w",
			admission.InvocationID, admission.Backend, record.Generation,
			record.Outcome, ErrBackendNotConformant)
	}
	if !record.ConfigurationBound() {
		return fmt.Errorf("admission %q backend %q conformance generation %d: %w",
			admission.InvocationID, admission.Backend, record.Generation,
			domain.ErrConformanceConfigurationUnbound)
	}
	if admission.BackendConfigurationDigest != record.ConfigurationDigest {
		return fmt.Errorf(
			"admission %q backend %q configuration %s differs from conformance generation %d configuration %s: %w",
			admission.InvocationID, admission.Backend, admission.BackendConfigurationDigest,
			record.Generation, record.ConfigurationDigest,
			domain.ErrAdmissionConfigurationMismatch)
	}
	if excess := domain.ExcessCapabilities(admission.Capabilities, record.Capabilities); len(excess) > 0 {
		return fmt.Errorf("admission %q backend %q claims %v beyond conformance generation %d: %w",
			admission.InvocationID, admission.Backend, excess, record.Generation,
			domain.ErrAdmissionExceedsConformance)
	}
	return nil
}
