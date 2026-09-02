# daemon

`freesided`, the Go daemon: event inbox, workflow engine, signet (attention service), StageDriver and ReviewSource, ward (runner layer), gauntlet (hostile import and clean verification), git/publish service, store, and sync API. It owns workflow state and all credentials; clients are thin (see `docs/plan.md` §5.1, §5.2).

Daemon CI builds and tests on **Linux as well as macOS from day one**: the daemon core takes no Apple-only dependencies, making portability continuously verified rather than aspirational (plan §3.3).

- **Toolchain:** Go (single static binary, supervised by launchd/systemd, dedicated user). Module `github.com/freeside-ai/freeside/daemon`, pinned in `go.mod`; build/test/run commands are in `AGENTS.md`.
- **Scope boundary:** daemon-side code only. The daemon/client contract is defined in `api/`; server-side code implementing it lives here, never hand-authored to diverge from the spec.
- **Status:** initialized in Phase 1A (Wave 0 unit 1). `internal/` holds one placeholder package per lane (`signet`, `export`, `importer`, `verify`, `publish`, `ward`, `domain`, `engine`); each lane's real code lands with its Wave unit.

## Testing conventions

**Golden files.** Tests that assert a serialized shape compare it against a
committed fixture rather than hand-writing the expected bytes inline. Use the
shared helper `internal/golden` so every lane's golden tests share one shape
and one regeneration switch:

```go
import "github.com/freeside-ai/freeside/daemon/internal/golden"

func TestRender(t *testing.T) {
    got := render(input)          // []byte
    golden.Assert(t, "render", got) // vs testdata/render.golden
}
```

- Fixtures live in the test package's own `testdata/` directory, named
  `<case>.golden` (the `name` passed to `Assert`).
- Regenerate after an intended change with the package-level `-update` flag,
  then review and commit the diff:

  ```sh
  go test ./internal/foo -run TestRender -update
  ```

`internal/golden` and its `golden_test.go` are the worked example.

**Timer-dependent tests.** A test whose behavior depends on real stdlib time
in the code under test (a `time.Timer`, `time.Ticker`, `time.After`,
`time.Sleep`, or a `context` deadline) runs inside a `testing/synctest` bubble
rather than a real-clock sleep or poll loop:

```go
import "testing/synctest"

func TestCadence(t *testing.T) {
    synctest.Test(t, func(t *testing.T) {
        // ... start the timer-driven work in a goroutine ...
        time.Sleep(5 * time.Second) // fake time; advances only when idle
        synctest.Wait()             // let the work settle before asserting
    })
}
```

The bubble's fake clock advances only when every goroutine in it is durably
blocked, so a ticker or timeout fires deterministically with no wall-clock
flakiness and no wasted real time.

- **Ratchet, not a retrofit sweep.** New or substantially revised
  timer-dependent tests use synctest; an existing real-sleep or `Eventually`
  test converts only when a revision already touches it.
- **Only where the code uses the real `time` package.** Behavior driven by an
  injected clock (the scheduler's occurrence-due `clock`, the janitor's
  reconciliation `now`, the engine's retry `now`) is already deterministic and
  gains nothing from synctest; leave those on their fake clock.

`internal/scheduler/run_synctest_test.go`, which drives `Scheduler.Run`'s real
`time.NewTicker` cadence, is the worked example.

**Durable transition matrix.** The production restart matrix injects process
loss on both sides of every registered durable boundary, closes and reopens the
same SQLite store, rebuilds the engine and disk-backed fake reviewer, and gives
retry-only states an explicit reconcile-pass bound. It runs under ordinary
`go test` with injected clocks and local fakes only.

- `internal/engine/durable_transition.go` is the engine registry.
  `internal/engine/specification_test.go` owns specification outcome and
  specification-approval rows; `internal/engine/operator_feedback_test.go`
  owns specification-answer and operator-feedback rows;
  `internal/integration/production_publication_test.go` owns verification,
  review request/result, publication, ready-item, and terminal rows plus the
  registry-completeness check.
- `internal/exec/stage/recovery_test.go` is the sibling registry for seed
  handoff and execution export. Its durable phases feed the deeper per-phase
  recovery fixtures that assert single handoff, credential attachment, export,
  and outcome effects.
- `internal/integration/workflow_engine_test.go` owns the pre/post matrix for
  atomic AttentionItem resolution.

When adding a durable transition, add its closed-set engine constant (or the
stage sibling row), place nil-default before/after hooks immediately around the
persistence transaction or external effect, register both crash sides in the
owning matrix, and assert the exact identities and effect counts applicable at
that stage. A transition is incomplete until the engine completeness test or
the owning sibling registry names it. Policy/profile and reviewer-configuration
drift remains covered by the adjacent fail-closed recovery fixtures; a new
drift axis needs both a preterminal refusal and a terminal-fact adoption case.

## GitHub App Credential Onboarding

The default publish identity is one public GitHub App owned by the operator's
personal account. A fresh operator registers it through GitHub's manifest flow;
Freeside generates the suggested name, pins the publish permission set, and
writes the conversion key directly to the protected credentials directory.

Repository onboarding uses GitHub's native installation page:
`https://github.com/apps/<app-slug>/installations/new`. Select only the
repository being onboarded. For an organization, GitHub may turn that action
into a request for an organization owner's approval; after approval, resume
onboarding and Freeside detects the installation through canonical
App-authenticated discovery.

Every machine uses a distinct private key within the same registration. On a
new machine, open the App's personal-account settings page, generate a private
key, and import the downloaded PKCS#1 PEM. Freeside authenticates the key
against the recorded numeric App ID, owner, canonical name and slug, and
visibility before protected storage accepts it, then records the same SHA-256
public-key fingerprint GitHub displays. Delete the downloaded PEM after the
import succeeds. Copying a PEM between machines is outside the contract because
it prevents independent machine revocation.

Publish-credential doctor checks cover the keystore layout and owner-only
modes, expected per-registration key presence, canonical visibility metadata,
active janitor coverage for every registration, and the former singleton
layout. Reusing one key across multiple local registrations is also reported,
the PEM-copy pattern that can be detected from one machine.

## Operational Commands

`freesided setup -operator <login> -operator-id <id>` creates the Phase 1A
single-directory layout and the canonical empty installation-authority
document. The default is `~/.freeside`; `-config-dir` selects another root.
The first pass returns GitHub's manifest form for the operator's public
personal App. After GitHub returns the one-time code, repeat setup with
`-registration-code-stdin` and supply the code only on standard input. For an
interactive shell, capture it without echo and pipe it through the shell's
built-in `printf`:

```sh
(
  set +x
  unset FREESIDE_MANIFEST_CODE
  read -r -s FREESIDE_MANIFEST_CODE
  printf '\n'
  printf '%s\n' "$FREESIDE_MANIFEST_CODE" | freesided setup \
    -operator <login> -operator-id <id> -registration-code-stdin
  unset FREESIDE_MANIFEST_CODE
)
```

Non-interactive automation can redirect an owner-only secret file or
secret-manager descriptor instead:

```sh
freesided setup -operator <login> -operator-id <id> \
  -registration-code-stdin < /run/secrets/github-app-manifest-code
```

Neither form places the code in the `freesided` argument vector, environment,
command history, or shell trace. Freeside verifies GitHub's canonical App,
visibility, permissions, and owner identity, stores the private key directly
in the protected keystore, and creates the registration's fail-closed authority
entry. If setup is interrupted after conversion, rerun it without a code or
registration-code flag; the pending-authority marker resumes only that exact
conversion. A replay validates existing authority without overwriting it, and
existing credentials without matching authority fail closed rather than
silently authoring an empty destructive installation set.

Every state directory is owner-only, including the corrected
`<db>.fake-stage-driver` path required by the attended publication bootstrap.
The static binary and supervisor may be installed through a one-time narrow
elevation step, but the stateful daemon always runs as the non-root operator.

### Enroll A Codex Subscription Identity

`freesided enroll-codex` bootstraps a Codex subscription identity and repairs
one whose refresh chain was revoked. It replaces the live auth store only
while holding that identity's mutation lease, immediately spends the supplied
refresh token in a real provider rotation, verifies an access-only agent
snapshot, and binds the resulting store digest and expiry to a recovery
attention item. It never prints or persists token bytes in the database.

Stop `freesided` before running this direct-store maintenance command. Create
two non-overlapping owner-only directories: a temporary input root for the
fresh `codex login` result and the durable review-input root that will contain
the daemon-owned live auth store. The command refuses group/world-accessible
directories, paths outside those roots, symlink escapes, and an input file
that is not an owner-only regular file.

```sh
install -d -m 700 /path/to/codex-enrollment-input
install -d -m 700 /path/to/freeside/review-inputs
codex login
install -m 600 ~/.codex/auth.json /path/to/codex-enrollment-input/auth.json

freesided enroll-codex \
  -db /path/to/freeside.db \
  -project <project-id> \
  -auth-identity codex-primary \
  -input-root /path/to/codex-enrollment-input \
  -input-file /path/to/codex-enrollment-input/auth.json \
  -auth-store-root /path/to/freeside/review-inputs \
  -auth-store /path/to/freeside/review-inputs/codex-primary.json \
  -approved-recipe sha256:<approved-verify-recipe-digest>
```

Pass `-approved-recipe` once per approved verification-recipe digest the
daemon runs with, the same set given at daemon start. The command opens the
store and re-gates its recipe-gated evidence against this set exactly as the
daemon does, so any production store, which has recorded such evidence, needs
it; omitting it fails before enrollment with a message naming the flag. A
fresh store with no recipe-gated evidence needs no `-approved-recipe`.

Success prints only the identity, store path, lease fence, verified digest,
access-token expiry, and recovery item coordinates. The input refresh token
has then been deliberately spent and is no longer a usable backup. Securely
remove the temporary input copy after success; preserve the rotated live store
at the printed path. A retry after verification safely rechecks and projects
that same store even if the temporary input has already been removed.

Restart the daemon with `-review-auth-mode subscription`,
`-review-auth-identity codex-primary`, `-review-input-root` set to the durable
review-input root, and `-review-auth-snapshot` set to the live store path.
Initial enrollment and recovery both leave the identity blocked until the
operator inspects the displayed digest, fence, and expiry and accepts the
item's `Resolve re-enrollment` action. That command-backed decision, not this
maintenance command alone, clears the revoked-identity marker.

`freesided onboard <owner/name>` packages the previously manual path. It:

1. resolves the repository ID through exactly one selected installation across
   every local App registration in the operator-authored authority document,
   refusing a second App binding that runtime resolution would find ambiguous;
2. records a bounded, non-authorizing pending installation or
   repository-expansion envelope, returns the canonical native GitHub
   installation route, and resumes the same envelope with `--resume`;
3. mints a pending-gated, repository-scoped read-only token, uses it for
   private-repository identity and clone requests, audits the live `-base-ref`,
   requires its resolved commit to equal `-commit`, and retains that exact
   evidence;
4. derives a conservative profile from the fresh retained audit and returns
   the installation- and image-request-bound approval digest as
   `review_required`; the `-commit-plan` owner-policy flag selects
   `single_commit` (the conservative default) or `plan_preferred`;
5. accepts only `-approve <approval_digest>` from that review; and
6. invokes `internal/projectimage` directly, requires the returned preparation
   command and registry/image destination to match the approved request, then
   rechecks the exact authority and reconciled grant before activating the
   profile; a pending installation is promoted only after the image has been
   durably recorded and the review accepted.

The selected npm recipe is detected at `.freeside/verify.json` under `-source`,
or may be supplied explicitly with `-recipe`. Run the command once without
`-approve`, inspect the complete JSON review, then repeat it with the returned
approval digest. A changed workflow head or installation coordinate produces a
different review, as does replacement of the bounded pending intent, and
invalidates the earlier approval. That is the one Freeside manual review; a
GitHub organization approval remains a separate native prerequisite.

`freesided doctor -db <path> -backend-configuration-digest <digest>` reports
the current config-bound conformance and workspace-handoff declaration plus
checkpoint encryption, checkpoint currency, artifact closure, and restore-test
age. Pass each current policy digest as a repeatable `-approved-recipe`; the
same set gates both artifact reconstruction and checkpoint closure. Each
unhealthy dimension converges on one blocking `system_health` item; a later
healthy pass resolves that item. Production driver mode supplies its live
backend digest and runs the same composition at startup and every 24 hours by
default (`-doctor-interval` overrides the cadence). The production daemon
accepts the same repeatable `-approved-recipe` flag and supplies that exact set
to persistence reconstruction and every scheduled doctor pass. Scheduled
conformance holds the janitor's latest completed coverage stable for its
authenticated fetch, so the janitor's deliberate mid-pass withdrawal cannot
terminate the daemon. Doctor applies the selected mode's full capability floor; unattended
health therefore requires the networkless-export and enforced-provider-egress
proofs in addition to the attended handoff floor. Pass `-operating-mode
unattended -review-configuration-digest <digest>` to a one-shot doctor for an
unattended daemon. Use the `digest` field from that daemon's startup log record
whose message is `effective reviewer configuration`; it is computed from the
same effective review image, model, auth, instruction, and workspace inputs
that unattended admission enforces. The default mode is `attended_dev`, where
the review-configuration flag is not required.

`freesided preflight` is the production-composition gate used by
`scripts/run-real-work.sh` before it submits work. Its deterministic JSON
manifest binds the exact database schema, daemon build, listener, repository
and base, active profile, review configuration, source identities, seed,
image digests and tool capabilities, credential readiness, and build-egress
posture. Every check reports `passed`, `failed`, or `not_run` with evidence;
a failure also carries remediation and returns nonzero. The harness refuses
submission on failure and saves a passing, secret-free manifest by content
digest under `<state-root>/production-evidence/composition/`. Build-egress
reachability is explicitly `not_run`: this gate validates that configuration
without performing a live build-egress probe. Repository observation may mint
one short-lived, repository-scoped read token and records the required
credential audit row; every other observation is read-only. `freesided submit
--composition-manifest <path> --require-composition` then refuses any
submission whose manifest is not a passing one bound to the exact submitted
inputs and their deterministically derived run identity.

`freesided follow -db <path> -run <run-id>` follows one run's observed
timeline: submission, admission or hold, invocation start, terminal
collection and import, and final outcome. It prints each milestone as the
daemon records it, plus a status block (current hold, per-invocation status
and liveness, elapsed time, last observation) whenever the observed state
changes. The timeline is durable, so a disconnected or restarted follow
resumes with everything already observed; `-once` prints one snapshot instead
of following. Liveness distinguishes an observed live invocation from an
`observation_gap`: a stopped daemon leaves its last observation behind, and
the reader's `-freshness-window` (30s by default) is what turns that stale
observation into a gap rather than a claim. Holds and definitive blocks
display the contract's closed reason codes; no free-text reason exists to
show, and there is no percentage complete anywhere.

The follow exits when the run's outcome is decided: `published`, `blocked`
(with the block's reason code), `failed`, or `lost`. A completed execution is
not yet an outcome, so a run awaiting publication (including one an attended
daemon holds by design) keeps following until the operator interrupts, which
reports the outcome as `pending`. Exit status reports whether the run could
be observed, never the run's own verdict: following a blocked or failed run
to its outcome succeeds.

Following is a read of the daemon's own durable observation projection over
the same direct-store transport as `freesided submit`. It never reads a live
writer's filesystem, stdout, stderr, or transcript; see the Run Observation
Contract below for that boundary and its limits.

**Follow is the pull diagnostic, not the way a stall reaches an operator.**
Freeside's premise is that nobody is watching: work that needs judgment
raises an attention item, and a stalled run gets the ward- or daemon-observed
stall heartbeat and its notice (plan §5.12, phase 1B.1). Follow answers a
different question, the one a push channel structurally cannot: what is the
state of *this* run right now, including the case where the daemon itself
stopped and therefore raised nothing (`observation_gap` exists exactly for
that). Reach for it after a notice, when an expected pull request has not
appeared, or while bringing an unattended configuration up; `-once` is that
question in its plainest form. It is not a substitute for the stall notice,
and a stall an operator only learns about by watching a terminal is a gap in
the notice path, not a use for this command.

The long-running loops report through a structured logger on stderr, which a
supervisor captures to a file. Records carry a severity, a `subsystem` key
(`engine`, `scheduler`, `active-resource`, `claude-driver`), and, where the
loop has them in scope, `run` and `invocation`, so a recurring failure can be
filtered out of the stream rather than read out of it. `-log-level` selects
the severity, default `info`; the accepted spellings are slog's own (`debug`,
`info`, `warn`, `error`). Every per-iteration record sits at `debug`, so the
default emits nothing per pass: these loops tick on fixed cadences forever,
and a record per idle pass buries the ones worth reading. Credential values
never reach a record; `publish.Secret` renders as `[REDACTED]` through every
formatting path a record can take, which `cmd/freesided`'s logging test pins.

Logging is diagnostic and does not replace the attention path: work needing
judgment still raises an attention item, and nothing depends on an operator
watching stderr.

Operational results always state the operating mode and isolation class.
`attended_dev` is the default; unattended operation is an explicit
`-operating-mode unattended` choice. An attended admission resolves the
currently active trust profile at import, where its protected paths are
applied; an unattended admission remains bound to the exact profile digest it
recorded before execution.

## Run Observation Contract

The run-monitoring contract (issue #394; plan §8) lets an operator client
follow an unattended run from submission through admission or hold,
invocation start, terminal collection and import, and final outcome. The
model lives in `internal/domain/observation.go`; the read surface is
`store.ReadTx.ObserveRun`, consumed over the same direct-store transport as
`freesided submit` (the client opens the daemon's SQLite database; the
timeline is persisted, so reconnect and daemon restart preserve it). Its
first consumer is `freesided follow` (issue #409), whose display lives in
`internal/observe`. That package is the whole verb, and its imports are held
to a closed allowlist that names no way to open a file, start a process, or
open a socket, so the containment below is structural rather than a promise;
the `cmd/freesided` file is a shim supplying streams, interrupt, and exit
code. The database is reached only through `internal/observe/observedb`,
whose exported surface is open, read one run's aggregate, and close, so no
write, checkpoint, restore, or backup-file capability is in the follow path
at all.

- **Milestones** are an append-only, first-observation-wins timeline of
  typed events (`run_submitted` through `publication_ready` or
  `publication_blocked`), written inside the transactions that commit the
  underlying workflow facts.
- **Holds** carry a closed reason-code vocabulary
  (`domain.AllRunHoldReasons`). There is no free-text reason field by
  design: codes are the entire operator-facing cause, so credentials,
  provider output, specification and policy content, and paths are
  unrepresentable in the observation surface (the `publish.MintRecord`
  precedent). The richer prose stays on the separately gated attention
  items.
- **Liveness** is derived, never stored: `DeriveInvocationLiveness`
  classifies the last observation (status, live bit, daemon-clock instant)
  against a freshness window, so a stopped daemon or unobservable runtime
  reads as `observation_gap` structurally, and elapsed time and last
  observation derive from the timeline. No percentage-complete field exists
  or can be added without a contract change.

### Adjudication Dispatch Telemetry

The machine-readable supervision snapshot (`freesided follow -snapshot`) carries a
bounded `adjudications` projection: one entry per finding per adjudication round
and revision, re-gated through the store's `ListFindingAdjudications` accessor
(every row is reconstructed and its content address, binding, and successor chain
revalidated before projection). It answers one calibration question without raw
SQLite access: how often do critical/high-severity, material, in-surface findings reach
deterministic engine dispatch versus model residue? It is telemetry, never
authority, and carries no prose — no rationale, evidence, cited rules,
assumptions, alternatives, open questions, finding message, or raw text.

Each `adjudications[]` entry carries:

- `attempt_number`, `round`, `revision`, `finding_id` — the stable join keys
  (with the snapshot's own run id). Join `review_yield` to `adjudications` on
  `(run id, round)`, never on a digest.
- `producer` — `engine` (a deterministic fast-path routing fact), `model` (a
  model-residue proposal), or `engine_model` (a model goal judgment composed with
  the engine's allowed-remediation authority). This is the dispatch axis.
- `route` — the adjudicated route (`remediate`, `park_separate_work`, …).
- `adjudication_confidence` — the entry's model-proposal confidence, present
  exactly on `model`/`engine_model` producers and null on a pure `engine` fact.
  It is never the classifier's confidence.
- `finding_severity` — the raw reviewer severity (`P0`…`P3`, empty when absent);
  the critical/high severity axis is `P0` or `P1`.
- `classifier_materiality`, `classifier_confidence` — the classifier's own tokens
  for the finding at the round's classification version, empty when no
  classification is recorded. `classifier_confidence` is deliberately distinct
  from `adjudication_confidence`.
- `in_surface` — whether the finding's location is contained in the run's
  resolved-policy declared paths, by the engine's own matcher. It is the
  declared-scope half of the engine's allowed-compatibility check and does not
  re-check tree existence (unreachable to a read-only observer), so an in-scope
  finding whose path is not yet in either tree still reads `true` — the
  case the calibration metric most needs visible. It is an independent property
  of the finding location, not a restatement of the entry's compatibility.
- `resolved_policy_digest` — the resolved policy the round was gated under; a
  change across rounds or attempts is a configuration change.

**Calibration rate.** Over each round's head revision (the entry with the
greatest `revision` for a given `(round, finding_id)`), take the population of
findings that are critical/high severity (`finding_severity` in `{P0, P1}` —
the operative content of plan §7's credibility guard, which today filters
nothing beyond the critical/high ceiling), material (`classifier_materiality`
at or above the analyst's chosen materiality bar, e.g. `{medium, high}` — the
bar is a parameter of the question, a threshold the analyst applies or sweeps,
not run data), and `in_surface`. The numerator is that population's findings
with `producer == engine`; the denominator is that population's findings with
`producer` in `{engine, engine_model}`. A denominator that routinely exceeds
the numerator is critical/high-severity, material, in-surface work reaching
model residue rather than the deterministic fast path — the signal an operator
watches.

Every per-finding input the predicate needs — severity, the classifier
materiality token, `in_surface`, and `producer` — is a projected field, so
given a chosen materiality bar the rate is computable without opening the
database; the analyst's bar is the only non-projected value, and it is a query
parameter, not run state. The engine's own per-run materiality and confidence
dispatch thresholds live in the resolved policy that `resolved_policy_digest`
identifies; they are deliberately not used to define this population, because
defining "material" by the engine's own bar would be circular and would mask
the threshold miscalibration this calibration exists to detect (projecting them
as interpretability context is tracked as #975).

Security limitations, stated for the operator surface:

- **No live writer output.** Monitoring consumes driver inspection
  (status and liveness) and the daemon's own durable records only. The
  writer's stdout, stderr, filesystem, and transcript are unreadable while
  the writer lives (`claude.Driver.Stream` returns an empty reader by prior
  decision); transcript drill-down remains on the post-teardown validated
  evidence path.
- **Observation is projection, never authority.** No recovery,
  publication, or teardown decision reads these rows; recovery re-observes
  runtime ownership and writer absence. Forging or deleting observation
  rows changes what an operator sees, not what the daemon does; readers
  re-validate every row and fail closed on anything the vocabulary cannot
  express.
- **The store transport implies local, daemon-equivalent access.** A
  reader of the daemon's database can read everything the daemon persists;
  the observation contract narrows what the *observation surface* carries,
  not what raw database access could reach. Remote exposure arrives only
  with the API unit that carries these shapes over `api/`.
