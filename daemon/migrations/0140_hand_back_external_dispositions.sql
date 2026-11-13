-- A hand-back cycle (issue #3160) answers the lander's conversation comment
-- the way an external review cycle answers an admitted reviewer's finding, so
-- its first round's outcome for that finding is the same record. Recreate the
-- 0090 guard with the hand_back origin admitted beside external_review; every
-- other condition is unchanged. The trigger keeps its name, so nothing is
-- dropped for good.
DROP TRIGGER external_finding_disposition_requires_admitted_cycle;

-- outbox.payload is a BLOB, which the JSON functions would read as JSONB, so
-- every read casts it to TEXT. The CASE fixes the evaluation order: a payload
-- that is not JSON is skipped before json_extract can raise on it.
CREATE TRIGGER external_finding_disposition_requires_admitted_cycle
BEFORE INSERT ON external_finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM findings AS raw_finding
    JOIN review_records AS record
      ON record.run_id = raw_finding.run_id
     AND record.round = NEW.round
    WHERE raw_finding.id = NEW.finding_id
      AND raw_finding.run_id = NEW.run_id
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
      AND json_type(raw_finding.body, '$.external') = 'object'
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
      AND json_extract(record.body, '$.base_sha') = record.base_sha
      AND json_extract(record.body, '$.head_sha') = record.head_sha
      AND record.head_sha = json_extract(raw_finding.body, '$.external.head_sha')
      AND EXISTS (
          SELECT 1
          FROM outbox AS authority
          WHERE authority.kind = 'publication_successor_authority'
            AND authority.status = 'dispatched'
            AND CASE WHEN json_valid(CAST(authority.payload AS TEXT)) THEN
                    json_extract(CAST(authority.payload AS TEXT), '$.run_id') = record.run_id
                    AND json_extract(CAST(authority.payload AS TEXT), '$.origin') IN ('external_review', 'hand_back')
                    AND json_extract(CAST(authority.payload AS TEXT), '$.review_round') = record.round
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.base_sha') = record.base_sha
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.head_sha') = record.head_sha
                ELSE 0 END
      )
      AND (NEW.disposition <> 'fixed' OR EXISTS (
          SELECT 1
          FROM review_records AS remediation
          WHERE remediation.invocation_id = NEW.remediation_invocation_id
            AND remediation.run_id = record.run_id
            AND remediation.round > record.round
            AND remediation.base_sha = record.base_sha
            AND remediation.head_sha <> record.head_sha
            AND json_extract(remediation.body, '$.run_id') = remediation.run_id
            AND json_extract(remediation.body, '$.round') = remediation.round
            AND json_extract(remediation.body, '$.base_sha') = remediation.base_sha
            AND json_extract(remediation.body, '$.head_sha') = remediation.head_sha
      ))
)
BEGIN
    SELECT RAISE(ABORT, 'external finding does not belong to an external review cycle round');
END;
