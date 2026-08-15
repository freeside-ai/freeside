-- Durable, write-once diagnostic detail for a definitively rejected export
-- (issue #768). When the gauntlet rejects a released export, the failed
-- execution_outcomes row keeps only a count in its summary and the released
-- directory is cleaned, so which findings tripped is otherwise unknowable.
-- This table records the per-finding detail (kind, path, and the finding's own
-- locating fields, all in the JSON body) so an operator can diagnose after the
-- fact.
--
-- It is diagnostic, not terminal authority: it sits beside the
-- execution_outcomes(failed) row the same rejection records, so there are
-- deliberately no exclusion triggers against execution_exports or
-- execution_outcomes. Like the other execution records it is daemon-internal,
-- so it lives on this write path with no entity_version/as_of_revision and is
-- not carried on the sync surface.
--
-- Write-once (putImmutable): a byte-identical replay of the same rejection
-- converges on the row; a different body for the same invocation is an
-- ErrImmutableConflict. The admission foreign keys make the invocation and
-- admission bindings the reconstruction cross-checks against real rows rather
-- than the caller's word.
CREATE TABLE export_rejections (
    invocation_id TEXT NOT NULL PRIMARY KEY
                       REFERENCES execution_admissions (invocation_id),
    admission_id  TEXT NOT NULL REFERENCES execution_admissions (id),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;
