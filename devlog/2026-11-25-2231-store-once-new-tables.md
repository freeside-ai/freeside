# New Tables Store Each Fact Once

Work unit: #3638. Ordinary trigger: a lasting convention that rejects a
reasonable option. The rule lives in `daemon/README.md` (Adding a
Migration), with a pointer in AGENTS.md (Daemon Coding Conventions); this
note records why.

## Decision: Plain Columns, No Second Copy

A table a migration creates gives each fact one plain column and keeps no
JSON body beside them. `0149_cordoned_run_settings.sql`
(`task_writer_classes`) is the worked example.

Chose plain columns over the store doc's old convention, a canonical JSON
body plus key fields copied by hand into columns:

- **Each copy costs a cross-check.** A field held in both places needs a
  decode check that the column matches the body, or a stale column goes
  unnoticed. On a Tier B row those checks are exactly what units must now
  remove when they touch the decode
  (`2026-11-07-1119-tier-b-tamper-ratchet.md`), so the shape creates work
  that the ratchet then undoes.
- **Nothing stopped new tables repeating it.** The store package doc
  prescribed the shape for every later lane, and
  `0142_settings_revisions.sql` and `0146_project_readiness.sql` both
  followed it this month: their bodies repeat the project id and revisions
  their columns hold. About a third of the migration files declare a body,
  document, or payload column.
- **Columns are what the database enforces.** Foreign keys, `NOT NULL`,
  and indexes act on columns, so a fact the database must guard belongs in
  one, and then a body copy of it adds only the cross-check above.

## Allowances

- **Nested values may stay JSON.** A JSON column may hold a value with no
  columns of its own, such as a list or an aggregate's embedded children,
  as long as no field in it is also a column. Without this the rule would
  contradict the store doc's embedded-children convention for synchronized
  aggregates.
- **Content-addressed records keep their body.** When a record's id or
  digest is computed from its body bytes, the body is the fact. Its fields
  aren't copied into columns by hand; a lookup column comes from the body
  through one write path or a non-key SQLite generated column. A primary
  key can't be generated, so a generated column serves lookup only.
- **Tier A gates stay.** An id rebuild or chain check that a Tier A gate
  relies on is unchanged (AGENTS.md, Worked Examples).

## What Stays

Existing tables keep their shape. Like the other daemon conventions, the
rule is a ratchet on new tables, not a sweep; reshaping an old table is a
data-rewriting migration with its own Tier A cost. No linter or test
enforces it; review does.

Revisit when a new table needs a whole-record body for a reason this note
doesn't cover, such as a synchronized aggregate whose snapshot must be
served byte for byte.
