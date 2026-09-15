# Bound and Canonicalize the Operator Task Name

Chose to reject an out-of-bound operator-supplied task name rather than repair
it, and to trim surrounding whitespace as the one exception. An operator name
is stored with source `operator`, which `store.SetTaskName` treats as
permanent, so a silently truncated or first-lined name would be a name the
operator never chose and can never fix. A 400 lets the operator shorten the
name and resubmit; trailing whitespace is never intended, so trimming it is a
safe repair, not a silent rewrite. Unit #1347, deferred from the #1346 review.

## Decisions

- **Reject, do not repair.** A name that fails the bound after trimming fails
  the submission and writes nothing. Rejected: truncate to 60 like the heading
  fallback (`taskHeadingName`), keep the first line like the publication title
  (`submissionTitle`), and drop the name like the agent path
  (`specification.go` title check). All three are fine where the name is
  refinable or advisory; none fit a permanent, operator-chosen field. The
  canonical form is one line of 1 to 60 code points (`utf8.RuneCountInString`,
  matching the agent and heading bounds), valid UTF-8, no credential-shaped
  token (`importer.ContainsSecret`).
- **The error text never contains the name.** The name may be the very secret
  the credential check refused, so `operatorTaskName`'s messages name the rule
  that failed ("blank", "multiline", "longer than 60 characters", "contains a
  credential", "not valid UTF-8") and never echo the input. The HTTP boundary
  surfaces the message verbatim, so this is a boundary requirement, not a
  nicety.
- **Enforce at the shared setter, not just the client path.** `SubmitTask`
  canonicalizes before any write, so an invalid name is refused whether the
  source creates or fetches a task, and never leaves a durable row. The shared
  setter in `SubmitSpecificationRunTx` re-runs the helper and fails closed, so
  a future caller that supplies a raw name cannot bypass the bound; on the
  client path the helper is idempotent on the already-canonical name.
- **Reject counts runes, the mock counts scalars.** The daemon and the mock
  agree only because the mock uses `unicodeScalars.count`, not `String.count`
  (grapheme clusters). A grapheme-based count would accept names the daemon
  refuses. The mock validator carries the length enforcement, and the mock has
  no secret scanner, so the credential check has no mock counterpart.
- **Schema states the rule in prose, not `maxLength: 60`.** The issue proposed
  `maxLength: 60` on `SubmitTaskPayload.name`; review (Codex, PR #1357) showed
  it diverges from the server. `maxLength` bounds the raw wire value, but the
  daemon and mock trim before counting, so a name padded with whitespace past
  60 raw code points yet within 60 trimmed is schema-invalid to a conforming
  validator while the server accepts it, a false client-side reject in the
  schema-stricter-than-server direction. No finite raw `maxLength` can express
  "trim, then <= 60", since surrounding whitespace is unbounded, and penalizing
  the whitespace the rule forgives contradicts this note's trim-is-the-one-
  repair choice and the unit's goal that schema and server enforce one rule.
  Owner-approved resolution: drop `maxLength`, keep `minLength: 1` (the safe,
  server-stricter direction), and let the prose description carry the full rule
  the daemon and mock enforce. Rejected making the daemon enforce a raw length
  to match `maxLength`: it would count trailing whitespace against the limit.
- **Reject invalid UTF-8 at the submit_task decode, not just in the helper.**
  `operatorTaskName` rejects invalid UTF-8, but `submitTaskCommand` decoded the
  arm with `TolerateInvalidUTF8`, which substitutes U+FFFD before the helper
  runs, so the rule was unreachable over real HTTP and only a Go `"\xff"`
  literal test (which bypasses the decode) exercised it. The submit_task arm
  now decodes with `RejectInvalidUTF8`, also requiring valid UTF-8 for
  `project_id` and `source`, with a raw-byte HTTP test on the wire path.
- **Contract door: direct fiat.** The unit edits `api/openapi.yaml`, a shared
  contract surface. The owner chose the `Handle #1347` fiat door (a direct
  cross-component unit), the same door #1346 used, over the spine-owned
  `kind:contract` door. Both were open because every open `kind:contract`
  issue is an unscheduled, unclaimed deferral. The generated consumers (schema
  mirror, `GeneratedSources`, both `ContractDigest`s) move in the same PR.

## Revisit When

- #1321 (task name history) becomes active. It rewrites `store.SetTaskName`
  and the permanence rule this decision relies on; if an operator name becomes
  editable, reject-over-repair loses its main justification.
- The composer gains a client-side length guard (a declared non-goal here).
  Today an over-long typed name shows the generic status-400 message; a
  friendlier limit belongs in `NewTaskSheet.swift`.
