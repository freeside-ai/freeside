package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Discovered model catalogs (migration 0127, issue #2594): what each Claude
// enrollment's pinned CLI last reported it can run. Daemon-internal
// observation that grants nothing (Tier B), so it is written on InternalTx
// and is not synchronized; a bad row fails the read loudly.

const (
	putDiscoveredModelCatalogSQL = `
INSERT INTO discovered_model_catalogs (enrollment_id, harness_build, observed_at, body)
VALUES (?, ?, ?, ?)
ON CONFLICT (enrollment_id) DO UPDATE SET
    harness_build = excluded.harness_build,
    observed_at   = excluded.observed_at,
    body          = excluded.body`
	getDiscoveredModelCatalogSQL = `
SELECT body
FROM discovered_model_catalogs WHERE enrollment_id = ?`
	listDiscoveredModelCatalogsSQL = `
SELECT body
FROM discovered_model_catalogs ORDER BY enrollment_id`
)

// RecordDiscoveredModelCatalog stores an enrollment's catalog, replacing any
// earlier one. The enrollment must exist: ErrNotFound otherwise.
func (tx *InternalTx) RecordDiscoveredModelCatalog(ctx context.Context, catalog domain.DiscoveredModelCatalog) error {
	fail := func(err error) error {
		return fmt.Errorf("record discovered model catalog %q: %w", catalog.EnrollmentID, err)
	}
	body, err := encode(catalog)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.GetClientEnrollment(ctx, catalog.EnrollmentID); err != nil {
		return fail(err)
	}
	if _, err := tx.tx.ExecContext(ctx, putDiscoveredModelCatalogSQL, catalog.EnrollmentID,
		catalog.HarnessBuild, formatTime(catalog.ObservedAt), body); err != nil {
		return fail(err)
	}
	return nil
}

// GetDiscoveredModelCatalog reads an enrollment's catalog: ErrNotFound when
// none was ever recorded.
func (tx *ReadTx) GetDiscoveredModelCatalog(
	ctx context.Context, id domain.ClientEnrollmentID,
) (domain.DiscoveredModelCatalog, error) {
	catalog, err := scanDiscoveredModelCatalog(tx.tx.QueryRowContext(ctx, getDiscoveredModelCatalogSQL, id))
	if err != nil {
		return domain.DiscoveredModelCatalog{}, fmt.Errorf("get discovered model catalog %q: %w", id, err)
	}
	return catalog, nil
}

// ListDiscoveredModelCatalogs reads every catalog in enrollment id order; one
// bad row fails the whole listing.
func (tx *ReadTx) ListDiscoveredModelCatalogs(ctx context.Context) ([]domain.DiscoveredModelCatalog, error) {
	catalogs, err := queryAll(ctx, tx.tx, scanDiscoveredModelCatalog, listDiscoveredModelCatalogsSQL)
	if err != nil {
		return nil, fmt.Errorf("list discovered model catalogs: %w", err)
	}
	return catalogs, nil
}

// scanDiscoveredModelCatalog decodes one row's body with plain validation; the
// copied columns serve the key and queries only.
func scanDiscoveredModelCatalog(row scanner) (domain.DiscoveredModelCatalog, error) {
	var body []byte
	if err := row.Scan(&body); err != nil {
		return domain.DiscoveredModelCatalog{}, notFoundOr(err)
	}
	return decode[domain.DiscoveredModelCatalog](body)
}
