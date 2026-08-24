package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Adapter conformance (plan §5.4 admission step 3, issue #894): the
// append-only log of what each adapter build's stage contract suite proved,
// on the backend_conformance pattern — the newest row per adapter digest is
// the current declaration, id order decides supersession, and the record
// must arrive unpersisted (generation zero). Dormant until the #867 cutover
// admits agents; the suite runner and admission gate land there, against
// this proven boundary.

// ErrAdapterGenerationSupplied is returned when an adapter conformance
// record arrives carrying a nonzero generation: the generation is this
// store's append identity, and accepting a caller-supplied one would let a
// writer forge where its proof sits in the supersession order.
var ErrAdapterGenerationSupplied = errors.New(
	"adapter conformance generation is store-assigned; a record must arrive with generation zero")

const (
	recordAdapterConformanceSQL = `
INSERT INTO adapter_conformance_records
    (adapter_digest, outcome, proved_capabilities, proved_at)
VALUES (?, ?, ?, ?)`
	// Newest row per adapter digest by id (insertion order), never by the
	// RFC3339Nano proved_at column: trailing zeros are trimmed, so
	// sub-second instants misorder lexicographically.
	latestAdapterConformanceSQL = `
SELECT id, outcome, proved_capabilities, proved_at
FROM adapter_conformance_records WHERE adapter_digest = ? ORDER BY id DESC LIMIT 1`
)

// RecordAdapterConformance appends one completed pass's record and returns
// the store-assigned proof generation. Append-only by design: the log is the
// audit trail of every completed pass, and a repeated outcome is a real
// recorded pass, not a conflict.
func (tx *InternalTx) RecordAdapterConformance(
	ctx context.Context, record domain.AdapterConformance,
) (uint64, error) {
	if err := record.Validate(); err != nil {
		return 0, fmt.Errorf("record adapter conformance: %w", err)
	}
	if record.Generation != 0 {
		return 0, fmt.Errorf("record adapter conformance for %q: generation %d: %w",
			record.AdapterDigest, record.Generation, ErrAdapterGenerationSupplied)
	}
	capabilities, err := json.Marshal(record.ProvedCapabilities)
	if err != nil {
		return 0, fmt.Errorf("record adapter conformance for %q: %w", record.AdapterDigest, err)
	}
	result, err := tx.tx.ExecContext(ctx, recordAdapterConformanceSQL,
		string(record.AdapterDigest), string(record.Outcome),
		string(capabilities), formatTime(record.ProvedAt))
	if err != nil {
		return 0, fmt.Errorf("record adapter conformance for %q: %w", record.AdapterDigest, err)
	}
	generation, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record adapter conformance for %q: %w", record.AdapterDigest, err)
	}
	if generation <= 0 {
		return 0, fmt.Errorf("record adapter conformance for %q: assigned generation %d: %w",
			record.AdapterDigest, generation, domain.ErrNonPositive)
	}
	return uint64(generation), nil
}

// LatestAdapterConformance reconstructs an adapter build's current
// declaration: the newest appended record, with presence reported separately
// because an empty log is the legitimate "never proved anything" state. The
// decoded record re-runs domain validation, so a tampered row claiming an
// unregistered capability fails closed instead of reconstructing as a wider
// declaration.
func (tx *ReadTx) LatestAdapterConformance(
	ctx context.Context, adapterDigest domain.Digest,
) (domain.AdapterConformance, bool, error) {
	row := tx.tx.QueryRowContext(ctx, latestAdapterConformanceSQL, string(adapterDigest))
	var (
		id           int64
		outcome      string
		capabilities string
		provedAt     string
	)
	err := row.Scan(&id, &outcome, &capabilities, &provedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.AdapterConformance{}, false, nil
	case err != nil:
		return domain.AdapterConformance{}, false,
			fmt.Errorf("adapter conformance for %q: %w", adapterDigest, err)
	}
	if id <= 0 {
		return domain.AdapterConformance{}, false,
			fmt.Errorf("adapter conformance for %q: row id %d: %w",
				adapterDigest, id, domain.ErrNonPositive)
	}
	var proved domain.LaunchCapabilitySet
	if err := json.Unmarshal([]byte(capabilities), &proved); err != nil {
		return domain.AdapterConformance{}, false,
			fmt.Errorf("adapter conformance for %q capabilities: %w", adapterDigest, err)
	}
	at, err := parseTime(provedAt)
	if err != nil {
		return domain.AdapterConformance{}, false,
			fmt.Errorf("adapter conformance for %q proved_at %q: %w", adapterDigest, provedAt, err)
	}
	record := domain.AdapterConformance{
		AdapterDigest:      adapterDigest,
		Outcome:            domain.ConformanceOutcome(outcome),
		ProvedCapabilities: proved,
		Generation:         uint64(id),
		ProvedAt:           at,
	}
	if err := record.Validate(); err != nil {
		return domain.AdapterConformance{}, false,
			fmt.Errorf("adapter conformance for %q: %w", adapterDigest, err)
	}
	return record, true, nil
}
