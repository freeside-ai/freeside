// Package migrations holds the daemon's ordered schema migrations, embedded
// into the binary so a deployed daemon carries its own schema history (plan
// §5.2: single static binary). It is spine shared-contract territory: schema
// changes land only through kind:contract work units.
//
// Files are named NNNN_description.sql with NNNN contiguous from the first
// file's version; the store applies pending files in order, one transaction
// per file, recording each version in schema_migrations (see
// daemon/internal/store). Migration files must not contain BEGIN/COMMIT (the
// store owns the transaction) and, once merged, are immutable: fixing a
// migration means adding a new one.
//
// The first file is a baseline: 0120_baseline.sql is migrations 0001 through
// 0120 squashed into one generated file (#3252). A new database applies it as
// version 120. squashed_history.txt lists the rows, one "version name digest"
// line each, that a database recorded for the files it replaced, so a
// database that applied them still opens; the store refuses one whose history
// stops below the baseline. daemon/README.md (Squashing Migrations) has the
// squash rule.
//
// Contiguous numbering is deliberately hostile to silent parallel
// authorship. Two branches may both take the next number; the contiguity and
// digest checks turn the collision into a hard error once one has landed,
// instead of a silent ordering ambiguity. The branch that lands second renames
// its file behind the other at rebase, and every migration declares each
// schema name it removes on a "-- drop: <kind> <name>" line, which the store's
// declared-drop test checks. daemon/README.md (Adding a Migration) has the
// steps.
package migrations

import "embed"

// FS holds the migration files and the squashed history. The store consumes
// it via Migrate; tests substitute an fstest.MapFS to exercise failure paths
// without checking in broken SQL.
//
//go:embed *.sql squashed_history.txt
var FS embed.FS
