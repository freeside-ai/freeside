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
- **Contract digest:** `scripts/api-contract-digest.sh` computes the contract identity, `sha256:` plus the lowercase hex SHA-256 of `openapi.yaml`'s exact bytes, and `--write` writes it into two compiled-in constants (`daemon/internal/signet/contract_digest.go`, `app/Sources/FreesideAPI/ContractDigest.swift`). The daemon reports it on `GET /health` (`contract_digest`); the Mac client compares it with its own to diagnose a client/daemon build skew instead of a silent sync failure (#1265). `scripts/check.sh api digest` (which runs `--check`), the daemon's `TestContractDigestMatchesSpec`, and the app's `generate` drift check all fail when the spec changes without regenerating the constants; `app/scripts/generate-api-client.sh` runs `--write` so a client regeneration refreshes them together.
- **Validate:** from the repo root:

  ```sh
  go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml --details --fail-severity warn api/openapi.yaml
  ```

  `api/vacuum.ruleset.yaml` documents the two deliberately disabled rules. CI runs the same lint invocation (`.github/workflows/api-ci.yml`), but via a pinned prebuilt vacuum binary rather than `go run` (compiling it from source cost ~7min per run); keep the pinned **version** in step across this command, the workflow, and `scripts/check.sh`, and update the workflow's binary **sha256** whenever the version changes.

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
refresh age.

Four `/commands` kinds act on them. None carries `expected_entity_version` or
`expected_bindings`. Each records an immutable receipt in the transaction that
applies it, and an exact replay of its `command_id` returns that receipt and
revision. A `command_id` names one command of any kind.

- **`refresh_usage`** asks for a pool's usage to be read now. New readings
  arrive in the pool's usage record, never in the receipt. An unknown pool is
  404.
- **`reassign_collector`** names the enrollment that reads a pool's usage. An
  enrollment not on that pool is 400.
- **`change_task_line`** sets one role's line on a task, as `freesided
  task-line` does, or clears it when `agent` is null or omitted. The record
  carries `recorded: false` when the role already had that line, and `line`,
  the role's line afterwards or null when it has none. A role whose attempt
  runs is 409 with the current task snapshot and sync epoch.
- **`set_routing_policy`** replaces the whole policy, as `freesided routing
  set` does. `expected_revision` is the revision the client saw, null or
  omitted when it saw none. Any other current revision is 409 with the
  current policy, so two operators can't overwrite each other. An agent
  outside the daemon's agent tree is 400.

Until #1720 wires the collection lifecycle, the daemon answers `refresh_usage`
and `reassign_collector` with 500 and records nothing.
