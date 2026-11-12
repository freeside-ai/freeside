# api

The OpenAPI spec: the single source of truth for the daemon/client boundary (`docs/plan.md` §5.14).

Task lifecycle has its own display vocabulary: `active`, `finished`, `stopped`,
and `abandoned`, plus null for unaffected work without a run. Confirmed queued
work can be stopped without a run or current position. Cancellation remains a
separate record; neither its acceptance nor a finished run proves quiescence.
The task's `wip` field supplies admission membership independently of display.

- **Toolchain:** OpenAPI (spec only).
- **Scope boundary:** the spec and nothing else. Generated code lives with its consumers (`daemon/`, `app/`), never here. Any spec change is treated as a migration.
- **Status:** `openapi.yaml` holds the **provisional** schema for the §5.14 sync surface (bootstrap snapshot, attention/runs/conversations read surfaces, ClientCommand submission, digest-addressed attachment upload), provisional until exercised by real clients (plan §11 Wave 0; decision record in `docs/history/decisions.md`). The daemon serves this surface (`daemon/internal/signet/http.go`) and the Swift client consumes the generated client under `app/Sources/FreesideAPI`. Schemas mirror `daemon/internal/domain` field-for-field; entity examples are lifted from that package's golden files so the linter proves they validate.
- **Contract digest:** `scripts/api-contract-digest.sh` computes the contract identity, `sha256:` plus the lowercase hex SHA-256 of `openapi.yaml`'s exact bytes, and `--write` writes it into two compiled-in constants (`daemon/internal/signet/contract_digest.go`, `app/Sources/FreesideAPI/ContractDigest.swift`). The same files carry the spec's compatibility level and floor (Compatibility Level below), so `--write` refreshes all three. The daemon reports the digest on `GET /health` (`contract_digest`) to name its contract; no client treats a digest difference alone as a skew. `scripts/check.sh api digest` (which runs `--check`), the daemon's `TestContractDigestMatchesSpec`, and the app's `generate` drift check all fail when the spec changes without regenerating the constants; `app/scripts/generate-api-client.sh` runs `--write` so a client regeneration refreshes them together.
- **Validate:** from the repo root:

  ```sh
  go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml -f api/functions --details --fail-severity warn api/openapi.yaml
  ```

  `api/vacuum.ruleset.yaml` documents the two deliberately disabled rules and the custom rules that `-f api/functions` loads (Response Tolerance below). `scripts/check.sh api policy` lints each fixture under `api/testdata/policy/` and fails unless the fixture fails the rule its first line names, so a custom rule that stops matching fails CI. CI runs the same lint invocation (`.github/workflows/api-ci.yml`), but via a pinned prebuilt vacuum binary rather than `go run` (compiling it from source cost ~7min per run); keep the pinned **version** in step across this command, the workflow, and `scripts/check.sh`, and update the workflow's binary **sha256** whenever the version changes.

## Response Tolerance

An app built from an older contract keeps decoding responses from a newer
daemon wherever the change only added fields; the daemon stays strict on
the way in.

- **Responses tolerate added fields.** An object schema a response can
  reach, through any chain of `$ref`s, leaves `additionalProperties` open,
  so the generated client ignores a field it doesn't know. Leave the
  keyword out rather than writing `true`, which makes the Swift generator
  add an `additionalProperties` property to the type.
- **Requests reject them.** An object schema only requests use sets
  `additionalProperties: false`. A schema both directions use
  (`AlternateAgentChoice`, `AlternativeChoice`, `TaskLineSelection`,
  `TaskProposalScope`) stays open in the spec, and the daemon's strict
  decoder still rejects an unknown field in it on the way in.
- **Responses tolerate added enum members.** Every string enum of two or
  more values that a response reaches lists `unknown`, and the app decodes
  a member it doesn't know as `unknown`. Unless the enum's description
  says otherwise, the daemon never sends `unknown` and rejects it in a
  request. A constant, a null marker, and a property a `oneOf`
  discriminates on are exempt: a new union arm is a breaking change.
- **Order matters in a union with no discriminator.** A client tries
  `CommandRejection`'s arms in order and takes the first that decodes, so
  an arm goes before any arm whose required fields are a subset of its own.
- **Enforced by the lint.** `api/functions/responseTolerance.js` backs the
  rules `response-schemas-tolerate-added-fields`,
  `request-schemas-reject-unknown-fields`, and
  `response-enums-list-unknown`. They read the unresolved
  document (`resolved: false`), because vacuum's resolved document inlines
  every `$ref` and so hides which schema a response reaches.

## Compatibility Level

An app keeps working against any daemon that still serves it, and refuses
plainly when it can't (#3109). Two integers under `info` in `openapi.yaml`
carry the rule; `scripts/api-contract-digest.sh` compiles both into the
daemon and the app beside the digest, and `GET /health` reports them as
`compatibility_level` and `compatibility_floor`.

- **The level counts breaking changes.** `x-compatibility-level` rises by
  one with a change that breaks an app built before it: a response field
  removed or retyped, a response enum member removed, a one-value response
  enum widened, a new arm in a response union, a request field made
  required or removed, a route or response status removed. An additive
  change, the kind Response Tolerance above makes safe, leaves it alone.
  It never goes down.
- **The floor is the oldest app a daemon serves.** `x-compatibility-floor`
  is the lowest app level a daemon built from the spec still serves. A
  breaking change raises it to the new level, unless the daemon
  deliberately keeps serving the old shape. It never goes down and never
  exceeds the level.
- **The app decides; the daemon never refuses on version.** An app whose
  level is below the daemon's floor stops syncing, sends no command, and
  asks the operator to update the app. An app whose level is above the
  daemon's level asks for a daemon update the same way. Every other pair
  syncs, with the feature list turning off what an older daemon lacks. A
  daemon that omits both fields reads as level 0, floor 0. The two fields
  and the rest of `HealthStatus` an app reads never change shape: an app
  that can't decode `/health` can't learn it was refused, and keeps
  syncing as if no verdict had come.
- **`scripts/check.sh api compat` keeps the level honest.** It runs
  `scripts/api-compatibility-check.sh`, which diffs `openapi.yaml` against
  its merge base with `origin/main` using `oasdiff` v1.35.0 and fails a
  breaking change that leaves the level unchanged, a level or floor that
  goes down, or a floor above the level. Locally the step needs `jq` and a
  fetched `origin/main`, and runs `go run github.com/oasdiff/oasdiff@v1.35.0`
  unless `OASDIFF` names another command; CI installs a checksum-verified
  binary of the same version (`.github/workflows/api-ci.yml`). Keep the
  version in step across the script, the workflow, and this line.
- **`oasdiff-severity.txt` sets the project's breaking set.** oasdiff
  reads it with `--severity-levels`; the file allows only `<check-id>
  <level>` lines, so its reasons live here. A new `oneOf` arm in a
  response (`response-body-one-of-added`, `response-property-one-of-added`)
  is pinned to `err`, which also stops oasdiff lowering it inside an
  `allOf`. A request property removed (`request-property-removed`, `warn`
  by default) is pinned to `err` too, because the daemon rejects a request
  body field it doesn't know, so an older app that still sends it fails. An enum member added to a response
  (`response-property-enum-value-added`,
  `response-mediatype-enum-value-added`) drops to `info`, because every
  response enum of two or more values lists `unknown`. That drop hides one
  real break oasdiff can't tell apart: a one-value response enum widened.
  The check doesn't catch it, so the author and reviewer raise the level
  by hand for one.

## Task Cancellation Contract

`stop_task` on `/commands` accepts a paired device's task ID, project ID,
expected sync epoch, and observed task snapshot version. It persists an
immutable receipt and task fence. The task's `cancellation` field is null or
carries `requested`, `failed_to_stop`, or `confirmed` independently of lifecycle
and WIP. Exact command replay returns the original receipt and revision.
A new command is accepted when its version is between 1 and the current
revision, so unrelated revision movement no longer rejects it; a stale epoch
or a greater version returns the current task snapshot and epoch.

This contract has no runtime acknowledgement producer or task Stop control.
Acceptance stays pending until #1368 supplies bound quiescence evidence;
#1344 owns stopped lifecycle/WIP and #1369 owns controls. No client may set
cancellation state. The mock also stays pending unless a test explicitly seeds
acknowledgement evidence. See [the plan](../docs/plan.md#59-durability-effectively-once)
for admission/publication ordering, late results, and restart reconciliation.

## Usage and Routing Command Contract

The bootstrap carries `usage_pools`, each pool's usage record, and
`routing_policy`, the current routing policy or null. Each record is written
at a new revision, so the heartbeat sees it change. A pool with no record yet
projects version 0 at the serving revision. Each window reading carries
`stale_at`, the earlier of its reset and `observed_at` plus the daemon's
refresh age, the same age collection uses: twice the doctor interval. Each
pool carries `recorded_exhausted_marks`, every exhausted mark its record
holds; a client lapses each by its `resets_at`, as it does `stale_at`, so the
list changes only with a write.

The bootstrap also carries `agent_catalog`, never null. Its `enrollments`
list every client enrollment the daemon holds, each with the models its
harness last reported (`discovery`), or null when discovery recorded none; a
new record moves the revision. Its tree part is the committed agent tree's
lineup, the selection per bound role; without a committed tree, `lineup` is
empty and `lineup_revision` is null. A client offers selections on a listed
enrollment that its discovery serves. The heartbeat carries the tree's
`lineup_revision`.
It changes only when the daemon restarts on a changed tree, which moves no
revision, so a client bootstraps when it differs from the adopted catalog's.

A task line on the wire is in one of two encodings, exactly one non-null: a
line recorded before selections existed carries its `agent` name with a null
`selection`, and every later line its `selection` (enrollment, model,
effort) with a null `agent`. A client writes selections only, in
`submit_task`, `change_task_line`, and a proposal start's `task_lines`. A
selection whose enrollment the daemon lacks, or whose recorded discovery
lists no such model, or no such effort for it, is 400 naming the field, such
as `task_lines[0].selection.effort`, never the text. An enrollment with no
recorded discovery passes on shape, and admission decides when the role
runs.

Five `/commands` kinds act on them. None carries `expected_entity_version` or
`expected_bindings`. Each records an immutable receipt in the transaction that
applies it, and an exact replay of its `command_id` returns that receipt and
revision. A `command_id` names one command of any kind.

- **`refresh_usage`** asks for a pool's usage to be read now. Once it accepts
  the command, the daemon starts a collection that begins after it, as
  `freesided auth refresh-usage` does, and never waits for it: the receipt
  comes first, and new readings arrive in the pool's usage record at a later
  revision, never in the receipt. During a failure backoff it starts none. An
  unknown pool is 404.
- **`reassign_collector`** names the enrollment that reads a pool's usage, as
  `freesided auth set-collector` does. The new collector commits with the
  receipt, at one revision, and a collection under it starts after.
  Naming the current collector records the receipt and nothing else. An
  enrollment not on that pool is 400.
- **`change_task_line`** sets one role's line on a task, as `freesided
  task-line` does, or clears it when `selection` is null. The record
  carries `recorded: false` when the role already had that line, and `line`,
  the role's line afterwards or null when it has none. A role whose attempt
  runs is 409 with the current task snapshot and sync epoch.
- **`set_routing_policy`** replaces the whole policy, as `freesided routing
  set` does. `expected_revision` is the revision the client saw, null or
  omitted when it saw none. Any other current revision is 409 with the
  current policy, so two operators can't overwrite each other. Each role
  orders enrollment ids in `enrollments`; an `agents` key is 400. An
  enrollment the daemon doesn't hold, one whose identity is disabled, one
  whose discovered models lack the role's lineup model and effort, and one
  repeated in a role are 400, by the role and the entry's position, never
  the id. `drain_first_account` names the usage pool that takes every new
  task ahead of reset order; null or omitted clears it, as a set without
  `-drain-first` does. A pool no ordered enrollment is on is 400. The same
  orders and account record nothing. A policy comes back with each role's
  `enrollments`, or the `agents` a revision set before policies ordered
  enrollments recorded, the other null.
- **`move_parked_task`** moves a task parked on its implementer's exhausted
  quota window to `choice`: an enrollment the park's hold offers in
  `alternate_agents`, with a model and effort its catalog serves, null for
  the lineup's. The daemon re-derives the offer against current state and
  refuses with 400 when the task is no longer parked, the enrollment no
  longer qualifies, or its catalog doesn't serve the model or effort,
  recording nothing. The record carries the choice, the alternate-agent
  retry record with the selection the choice resolved to, and the task lines
  the move set: the implementer on that selection, and each other role the
  routing policy orders an agent on that pool for, unless an operator set
  its line (`daemon/README.md`, Retry Or Move A Task To Another Agent).

## Restart Readiness Contract

`GET /restart-readiness` tells an update whether it can restart the daemon
now without cutting short running work. It returns the update drain in force
(or null), each kind of running work as a kind and a count, and `ready`.
`ready` needs a drain: without one, a launch can start the moment after the
read, so an empty blocker list alone proves nothing. The read changes no
state.

The daemon reports `execution_launch`, `review_launch`, and
`credential_lease` today, the kinds its store records. The other four kinds
(`verification_job`, `image_build`, `probe`, `publication`) exist only in the
running daemon, and #3104 adds them when it lets an update request a drain.
Until then no drain can be in force, so `ready` is never true.

A drain in force also shows on the sync surface as an `update_drain` entry in
`unattended_operation.stops`, with null `item_id` and `command_id` and `since`
set to when the drain began. The daemon starts sending it in #3104, in the
same change that makes it hold launches.
