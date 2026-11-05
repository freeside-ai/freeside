# Discover Claude Models From the CLI's Initialize Answer

Issue #2594; follows `2026-11-05-1035-select-by-enrollment-model-effort.md`
(plan revision 111). Tier A by path: the probe lives in `daemon/internal/ward/`
and runs under an enrollment's credential. The catalog record it produces is
Tier B ordinary state.

Chose the Claude CLI's answer to the control-protocol `initialize` request
over the `system/init` stream record, because only `initialize` answers
without a user message, so discovery never runs a model turn.

## Decisions

- **Chose `initialize` over the init record.** The 1035 note named "the
  Claude init record's `models`" as the source; the issue's planning
  correction found that record carries no `models`. A stream-json exchange
  that sends only `initialize` emits one record, the `control_response`,
  and the `system/init` record appears only once a user message starts a
  turn. The answer carries per-model `supportedEffortLevels`, which the
  per-model effort decision needs. Rejected: prompting a turn, which spends
  tokens and quota on every refresh and still yields no list.
- **Chose a second contained call over extending the enrollment proof.**
  `auth add` keeps its prompt probe (the proof that the token runs a turn)
  unchanged, and discovery is its own lineup-probe variant
  (`LineupProbe.Initialize`). Rejected: folding discovery into the proof,
  which would change the proof's command and output cap, and tie a
  catalog failure to enrollment success.
- **Chose to reuse the lineup-probe containment unchanged.** The credential
  is staged, read into a shell variable, and the stage deleted before the
  CLI runs; the export is refused if the stage remains; egress admits only
  `api.anthropic.com:443`. The variant changes only the CLI invocation and
  raises the output cap to 1 MiB for itself; the prompt probe keeps 64 KiB.
- **Chose `--safe-mode` for discovery.** It still answers `models` and about
  halves the answer (23 KiB against 45 KiB on CLI 2.1.318).
- **Chose to drop the `default` entry.** It is a pointer to another listed
  model, not a model; recording it would make a selection ambiguous about
  what runs.
- **Chose a non-synced bookkeeping record.** The catalog is one row per
  enrollment, replaced on each success, written through `InternalTx` with no
  revision bump. It is daemon state the selector reads, not a synced entity
  clients mirror; `auth list` is its display for now.
- **Chose to keep the last catalog on failure.** A failed refresh logs one
  warning; a missing catalog blocks a selection, never enrollment or the
  daemon.

## Live Findings

Run against the pinned agent image (CLI 2.1.282) with an invalid token:

- **The CLI contacts the provider while it starts.** It does so even with
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, which contradicts the
  issue's "without sending a provider request". The owner revised the live
  criterion to "no turn": exit 0, models present, and no `result` record,
  with the contact logged.
- **An invalid token in a fresh home gets a short, generic list.** The
  pinned image listed four models (`sonnet`, `claude-fable-5-1`, `opus`,
  `haiku`, the last without effort levels), where the issue's hand run from
  a logged-in home listed twelve. Whether a valid setup token in the
  container yields the fuller, per-account list is unverified; the owner
  holds that run.

## Refute-First Pass

A fresh-context reviewer tried to prove the probe and decoder wrong.

- **Confirmed, fixed: CLI text reached errors, logs, and the record
  unscanned.** The CLI holds the token, so a misbehaving build could print
  it into a subtype, id, or display name. The decoder now takes the token
  and refuses output carrying a run of it, as the enrollment probe
  withholds such a refusal, and no error quotes CLI text: validation names
  a model by index.
- **Confirmed, fixed: ids and levels were not checked as launch values.**
  An id such as `--x` would have become selectable. They now pass the
  check a launch applies to an option value; display names are bounded.
- **Confirmed, fixed: the live test logged stderr unscanned.** It withholds
  stderr that carries the token.
- **Confirmed, fixed: the store cross-checked copied columns.** Tier B
  decodes with plain validation, so the check is gone.
- **Disproved by inspection and the command golden:** the token never
  reaches argv or `printf`; the request constant survives `shellQuote`; the
  stage is deleted before both CLI calls; `$?` after the pipe is the CLI's;
  the prompt and Codex probe commands and the 64 KiB cap are unchanged;
  duplicate-key folding matches `encoding/json`; `auth add` cannot race the
  startup refresh because it refuses to run beside a daemon.

## Revisit When

- A CLI release drops or reshapes the `initialize` answer's `models`, or
  starts a turn for it.
- A valid-token run shows the list does not depend on the account; then a
  per-image catalog would replace the per-enrollment one.
- Clients need the catalog live, which would make it a synced entity.
