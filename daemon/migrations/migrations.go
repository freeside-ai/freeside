// Package migrations holds the daemon's ordered schema migrations, embedded
// into the binary so a deployed daemon carries its own schema history (plan
// §5.2: single static binary). It is spine shared-contract territory: schema
// changes land only through kind:contract work units.
//
// Files are named NNNN_description.sql with NNNN contiguous from 0001; the
// store applies pending files in order, one transaction per file, recording
// each version in schema_migrations (see daemon/internal/store). Migration
// files must not contain BEGIN/COMMIT (the store owns the transaction) and,
// once merged, are immutable: fixing a migration means adding a new one.
//
// Contiguous numbering is deliberately hostile to parallel authorship: the
// coordination protocol serializes migration work (kind:contract, exclusive),
// so two branches minting the same NNNN is a protocol violation, and the
// contiguity and digest checks turn it into a hard error at merge instead of
// a silent ordering ambiguity.
package migrations

import "embed"

// FS holds the migration files. The store consumes it via Migrate; tests
// substitute an fstest.MapFS to exercise failure paths without checking in
// broken SQL.
//
//go:embed *.sql
var FS embed.FS
