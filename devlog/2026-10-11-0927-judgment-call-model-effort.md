# Pass Each Judgment Call's Model and Effort, and Audit the Launch That Sends an Effort

Work unit #1619, plan §5.4 (Admission, call record). Follows the per-site
lineup lookup of #1425 (`devlog/2026-10-10-1602-judgment-lineup-lookup.md`)
and the first hand audit of the call launch
(`devlog/2026-09-09-2145-subscription-judgments.md`). The issue body carries
the contract. This note records the three owner decisions taken during
planning, the second hand audit, where the implementation left its plan, and
what the refute-first pass found.

## Owner Decisions (2026-10-11)

**The effort-capable call adapter ships with the launch change, over a
follow-up unit.** The baseline call adapter `claude_code_call_v1` declares
`harness_default` as its only sendable effort, and wardless admission refuses
an agent whose effort its adapter cannot send. A driver that can pass
`--effort` with no adapter that declares one would be unreachable from any
real lineup line, so the unit would have shipped a launch nothing could use
or test end to end.

**A second adapter, `claude_code_call_v2`, over replacing v1 in place.**
Changing v1's sendable efforts changes its digest. Every deployment that
adopted the judgment lines holds the v1 digest in a committed tree, and the
2026-09-09 audit record names it, so replacing v1 would leave those lines
unbound until each deployment re-adopted. v1 and its audit record stay; the
baseline call agent moves to v2; a new adoption gets a different digest for
that agent than an earlier one did.

**A native-default line keeps launching with `-judgment-model`, over passing
no `--model`.** The first audit covered a launch that always names a model,
and the driver refuses a completion from any other model than the one it
passed. Dropping `--model` for the native default would be a launch nobody
audited, and would leave the exact-match check with nothing to compare. What
a native-default line launches with once the flag is retired is decided with
the flag's removal.

**The call record gets no CLI-pin field.** #1425's note left this choice to
the owner. The run's preflight manifest and the startup digest check already
commit to the pin per run, and the record's audit entry names the adapter and
build admission accepted.

## The Second Hand Audit

The v2 record is earned by `TestPinnedClaudeCLIProtocol`, run on 2026-10-11
against Claude CLI 2.1.267 with a synthetic token and a localhost provider.
The binary was the published darwin-arm64 build, and its SHA-256
(`a681f300...43cd2558`) equals the `-judgment-claude-sha256` pin the owner's
real runs of 2026-09-11 recorded for 2.1.267, so it is the executable those
runs pinned. The probe now runs the production launch once for each effort v2 declares:
`harness_default` (no `--effort`), `low`, `medium`, `high`, and `max`. Every
run made one model request to the model passed, with an empty tool list,
`max_tokens` equal to the site's limit, thinking disabled, the task data as
the only input, and no repository content. Each run with an effort carried
it as the request's `output_config.effort`.

With no `--effort` the CLI still sends an effort of its own choosing: `high`
for the probe's model on this build. The call record's `effective_effort` is
`harness_default` for such a call: it states what the daemon sent, which is
what the treatment digest covers, and does not claim to know the harness's
default.

The audit says the launch stays one tool-less, thinking-disabled request at
every effort. It says nothing about what an effort does to answer quality or
length with thinking off. A higher effort may write longer answers and reach
a site's compute bound more often, which returns the site's fail-safe.

## Where the Implementation Left the Plan

- **`ClaudeCallAdapter()` returns v2 and `ClaudeCallAdapterV1()` returns v1,**
  where the plan kept the old name on v1 and added a new function. The
  unqualified name follows what the baseline tree uses, so the tree and the
  inference fake move to v2 through the name they already call. v1's digest
  is a literal in a baseline test, so the rename cannot move it silently.
- **Admission refuses a model or effort the launch cannot pass as one option
  value,** besides the driver's own check the contract asks for. An offer
  only has to name a non-empty model. With the driver's check alone, a line
  on an offer whose model holds a space or begins with `-` is admitted and
  then fails every call as `inference unavailable` with no health item. The
  client applies the same predicate (`inference.ValidLaunchValue`) at
  admission, so the role is unbound and reported. The predicate requires
  graphic characters, which also excludes format characters such as a
  right-to-left override, and bounds the value at 256 bytes, the bound
  stored identities already carry.
- **v2 writes its sendable efforts out,** where the plan read them from
  `domain.ClaudeCodeSendableEfforts()`. The audit covered exactly these
  efforts, so a table that gains one must not redefine an audited adapter. A
  baseline test holds the list to the table and fails when they part.
- **The role source picks the audit record by adapter digest and decides
  nothing.** Admission compares the digest and the harness build again, so a
  wrong pick is a refusal, never an admission.

## What the Refute-First Pass Found

An independent pass over the launch, client, and ledger change, prompted to
refute it.

- **Confirmed and fixed:** the unsafe-model path above, admitted and then
  failing quietly; and no length bound on a value that becomes a process
  argument.
- **Confirmed and closed by a test:** no client-level test sent a non-empty
  effort. One client over one real driver now launches two sites with
  different models and efforts and checks both launches and both records.
- **Left to the probe:** whether Claude CLI 2.1.267 accepts each effort the
  table can send. No code path can show that; the audit above does.
- **Allowed and stated:** an alias or short name as an explicit offer's
  `route_model_id` admits and then fails every call quietly, because the
  model check is exact and the role is admitted. The READMEs say so.
  Loosening the check by `identity_stability` is a design change for the
  owner.

A second pass covered the v2 adapter and the audit records.

- **Disproved by tracing and by test:** an unaudited call adapter admitting,
  and one audit record standing in for the other. The source hands over a
  copy of the record matching the adapter's digest, and admission compares
  digest and build again.
- **Confirmed and fixed:** v2 read its efforts from the table, so a table
  edit could redefine an audited adapter (above); no test held a record's
  audit date to its adapter, so swapped dates would have passed; and the
  policy README's steps for a role's own model left out the `agents.lock`
  entries, without which the revision is refused.

## Limits the Record States Instead of Hiding

- One effort table (`claude_code`) now serves two pinned builds: the ward
  image's and the call adapter's. If they ever accept different values, the
  table splits in `daemon/internal/domain`, which is a contract unit.
- A deployment that patches in a v2 agent and rolls the daemon back to a
  build without the v2 record loses that role: admission finds no proof and
  the health item says so.
- Nothing compares the deployed `-judgment-claude-sha256` pin with the
  audited build. Both audits cover 2.1.267 and no other build.

Revisit when the `-judgment-model` flag is retired (what a native-default
line launches with), when #1424 lands a conformance record for the call
launch (both hand audits and `judgmentCallAudits` go), or when the pinned
call build or the `claude_code` effort table changes (a new adapter and a new
audit, not an edit to v2).
