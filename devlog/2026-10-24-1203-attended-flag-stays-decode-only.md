# The Attended Flag Stays as a Decode-Only Field

Contract unit #2265, which carries plan revision 97 (no attended first run,
devlog/2026-10-24-0927-drop-attended-first-run.md) into the daemon. Mandatory
note: a contract change to the persisted admission snapshot. The agent
decided this in implementation, against the plan comment's assumption; the
owner reviews it in the PR.

## Decision

Chose to keep the binding's `attended` member as an optional, decode-only
field (`AdmissionAgentBinding.LegacyAttended`, `json:"attended,omitempty"`)
over dropping it from the struct. Admission never sets it and nothing reads
it. A binding recorded before this change decodes with the flag and
re-encodes to the bytes it was recorded as; a new binding omits it.

## Why

- **The binding is hashed, not only decoded.** The plan assumed that
  dropping the field was safe because `json.Unmarshal` ignores unknown
  fields. Decoding does succeed, but several readers then re-encode the
  decoded value and compare a digest: the execution admission's id is a
  content address over its canonical encoding, recomputed by `Validate` on
  every read; the ward recomputes a retained review outcome's completion
  evidence from the decoded result; `ReviewAdmission.Equal` compares
  encodings; and `PutReviewAdmission` replays compare stored bytes. With the
  field gone, every agent-bound admission recorded before the upgrade would
  fail its id check, and a resumed review would conflict with its own
  snapshot. The golden test's pinned pre-`selection_source` admission id
  caught it.
- **One field covers every hashing site.** The pattern is the one the
  binding already uses for fields it added (`route_model_id`,
  `selection_source`): omitted when empty, so older bytes keep their id.
  Here it runs the other way, keeping a removed member's bytes.

## Rejected Options

- **Drop the field (the plan).** Breaks reconstruction as above, which the
  contract requires.
- **Migrate stored rows.** Rewriting bodies changes admission ids and
  evidence digests that other records cite, so a migration would have to
  rewrite those citations too, across tables and retained evidence.
- **Derive the flag from the operating mode inside the canonical encoding.**
  Recorded admissions always agreed with their mode, so the id could be
  recomputed without storing the flag. It would need a separate canonical
  struct for the binding (one more place to keep in step with every field)
  and a per-site fix for the review digests, which have no mode to derive
  from.

## Also Decided

- **A leftover `agents/<agent>.attended` file is refused, not ignored**
  (planning, recorded on #2265): the loader fails closed on files outside
  the layout, and `freesided deputy` compares the tree with the baseline file
  for file, so a clear removal instruction beats an agent-name error.
- **Admission takes no operating mode.** With the mark and the flag gone,
  `resolveAgentAdmission`, `AgentSelection.CheckRole`, and
  `checkWriterRoles` no longer read the mode, so the parameter went with
  them. The mode's own requirements (plan §5.7) are checked elsewhere and
  are unchanged.

- **Holds stored under the retired check read back without their detail**
  (owner decision, 2026-10-24, in this session). Removing
  `agent_attended_mark` from the enum broke two readers of holds recorded
  before the upgrade: `GetRunHold` validates the stored check, so one such
  row failed Bootstrap and ListRuns for every client, and the app's cache
  decodes `AdmissionCheck` strictly, so a cached hold dropped the cache and
  its pending-command ledger. The owner chose to translate at both readers
  (the daemon read, and a fourth `CacheStore.migratingLegacyEncodings`
  translation) over keeping the value as a retired enum member, which would
  leave the API listing a check nothing produces, or translating in the
  daemon only, which loses the ledger once. That added
  `CacheStore.swift` and its test to #2265's scope.

## Revisit When

- No stored admission predates this change, for example after a store reset
  or a migration that rewrites admission ids anyway; the field can then go.
- No run hold or app cache predates this change; the two hold translations
  can then go.
