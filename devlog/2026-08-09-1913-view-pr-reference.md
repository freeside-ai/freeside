# Surface Durable PR References Without Rewriting Commands (#613)

## Chose: Structured Coordinates Over a Computed URL

Chose `pr_reference {repo, number}` over a persisted `html_url` because the
daemon already owns those exact coordinates in `ReadyItemPRBinding`. The ready
item copies them from the same trusted publication result that creates the
binding; clients may render the identity or compose the GitHub URL without
parsing presentation prose. The reference is required exactly on
`ready_for_final_review` items and explicit null on every other item type.

Rejected storing a URL because it would introduce a second computed
representation with no additional authority. Revisit when Freeside supports a
non-GitHub forge; an explicit host or URL can then extend the reference without
changing its identity fields.

## Chose: Preserve the `open_pr` Wire Token

Chose to retain the operator-invisible `open_pr` action token while rendering
"View PR", over the implementation comment's recommended `view_pr` rename.
Accepted commands are immutable, content-digest-bound records. Removing the old
enum member would make a previously valid persisted command invalid after an
upgrade, while an alias would create the silent dual vocabulary the issue
forbids. The app treats the action as navigation, opens the validated URL, then
records the unchanged action; a failed or unavailable navigation records no
false engagement.

Revisit when the storage contract has an explicit versioned migration mechanism
for immutable commands, or at a declared compatibility-breaking epoch.

## Chose: Authenticate Legacy Backfill in Go

Chose a one-time JSON-body migration over relying only on new-item construction.
Existing ready items predate `pr_reference`; requiring the field without a
backfill would make those rows fail domain reconstruction and disappear from
sync after upgrade. Production rows copy from the daemon-owned
`ready_item_pr_bindings` table. Older attended fake-publication rows have no
such binding, so migration 36 runs a Go hook in the same transaction: it uses
the shared fake-task and publication-record decoders, proves the deterministic
item ID and complete project/run subject, validates the frozen terminal digest,
and requires the dispatched intent and recorded outcome to agree before it
mints an anchor. Only then does the hook advance the synchronized revision and
rewrite anchored bodies.

Rejected expressing the fake authority proof in SQLite because JSON predicates
would duplicate only a weaker subset of the task and publication validators and
could not safely reproduce the terminal digest. Rejected leaving every legacy
fake row for manual repair because valid deployed rows have enough durable
evidence for deterministic recovery. Malformed or ambiguous history remains
unmodified and fails current reconstruction closed.

## Refute-First Dispositions

- **Rejected by verification, URL path injection:** domain and app contract
  validation require exactly two non-empty repository path components, reject
  dot segments, and require a positive PR number. URL construction appends
  components rather than interpolating an unchecked string. Focused tests cover
  missing, mistyped, non-positive, extra-component, and traversal-shaped input.
- **Rejected by verification, false command audit:** injected opener tests prove
  successful navigation records the existing action and keeps the item open,
  while URL construction or opener failure records nothing.
- **Rejected by verification, legacy data loss:** the migration test starts from
  a pre-migration ready-item body plus its durable binding, then proves the
  structured reference, revision advance, entity-version advance, and retained
  `open_pr` offer after reconstruction.
- **Confirmed and fixed, synchronized-body retargeting:** every ready item now
  receives an immutable store-owned PR-reference anchor in its creating write,
  and reconstruction compares both coordinates before synchronization.
  Production items additionally re-run the fully validated
  `ReadyItemPRBinding`; fake-publication items use the generic anchor because
  they have no production binding. Migration reconstructs a legacy fake anchor
  only through the shared Go contracts. The refutation corpus proves that a
  nondeterministic item ID, foreign project, malformed task, tampered terminal
  digest, foreign intent repository, or divergent outcome head cannot mint
  one. Fake-task recovery also recognizes the frozen pre-reference
  terminal-binding payload shape, then re-gates migrated coordinates against
  that same durable outcome.
- **Confirmed and fixed, asynchronous iOS open failure:** the platform URL
  opener is async and awaits `UIApplication.open`'s completion result before
  the navigation command is submitted. The app claims a shared, process-local
  per-item reservation before invoking the opener, so a recreated card cannot
  open a duplicate browser tab across that suspension. The reservation carries
  no command and never enters the replay ledger; only opener success advances
  to durable command registration, while failure or process exit cannot later
  replay false engagement. Delayed opener and store-boundary tests prove the
  cross-model exclusion, non-replayability, and failure-release paths.
- **Accepted by decision, live identity drift:** this navigation unit uses the
  observation-backed stored repository path and PR number; it does not perform
  the execution-time numeric repository-identity recheck deferred by
  `2026-08-05-0113-ready-identity-fail-closed.md`. Existing observation-time
  invalidation remains the authority until that defense-in-depth work is
  explicitly assigned.
