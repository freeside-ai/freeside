-- Review launches hold a usage pool slot (#2441).
--
-- A review admission holds a slot until its launch ends, and an admission that
-- never launched has no ward record to end it. A shadow review abandoned before
-- its launch writes a row here; so does this backfill for the shadow admissions
-- already abandoned that way, since a routed one still ends in a review record
-- or a review failure and a live shadow one predates its slot.
CREATE TABLE review_slot_releases (
    invocation_id TEXT NOT NULL PRIMARY KEY REFERENCES review_admissions (invocation_id),
    released_at   TEXT NOT NULL CHECK (released_at <> '')
) STRICT;

INSERT INTO review_slot_releases (invocation_id, released_at)
SELECT invocation_id, admitted_at FROM review_admissions
WHERE role = 'shadow_reviewer'
  AND invocation_id NOT IN (SELECT invocation_id FROM codex_review_requests)
  AND invocation_id NOT IN (SELECT invocation_id FROM shadow_review_records);
