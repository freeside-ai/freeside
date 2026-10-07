# daemon

`freesided`, the Go daemon: event inbox, workflow engine, signet (attention service), StageDriver and ReviewSource, ward (runner layer), gauntlet (hostile import and clean verification), git/publish service, store, and sync API. It owns workflow state and all credentials; clients are thin (see `docs/plan.md` §5.1, §5.2).

Daemon CI builds and tests on **Linux as well as macOS from day one**: the daemon core takes no Apple-only dependencies, making portability continuously verified rather than aspirational (plan §3.3).

## Protected Prompt Delivery

New Claude stage intents select `file_v1`. The renderer accepts at most 1 MiB
of complete UTF-8 input. Ward verifies its digest on a separate protected
volume before writer creation, journals the volume binding with launch state,
and supplies the root-opened file as Claude's user stdin. Existing intents
retain argument delivery and the original 31-KiB limit. This changes transport;
it neither retries a failed invocation nor grants successor publication.

The optional `TestPinnedClaudePromptStdinLive` probe uses a cached, pinned
Claude 2.1.220 image, synthetic text larger than the argument limit, and a mock
API on loopback inside a network-disabled container. It checks that the exact
text reaches a user-message block after privilege drop, and that the dropped
user cannot independently open the protected file. It uses no real provider
credential. Set `FREESIDE_PROMPT_STDIN_LIVE=1`, `FREESIDE_PROMPT_IMAGE` to the
digest-pinned image, `FREESIDE_RIG_ACQUISITION` to an authorized held rig's
acquisition JSON, and `FREESIDE_STATE_DIR` to that rig's state root, then run:

```sh
go test ./internal/exec/claude -run '^TestPinnedClaudePromptStdinLive$' -count=1 -v
```

The probe registers one fresh container under the held rig, then verifies
ownership before cleanup. Do not borrow an active exercise's rig without
authorization. Its token stays in process memory and must not be printed.

- **Toolchain:** Go (single static binary, supervised by launchd/systemd, dedicated user). Module `github.com/freeside-ai/freeside/daemon`, pinned in `go.mod`; build/test/run commands are in `AGENTS.md`.
- **Scope boundary:** daemon-side code only. The daemon/client contract is defined in `api/`; server-side code implementing it lives here, never hand-authored to diverge from the spec.
- **Status:** every lane in `internal/` holds real, tested Go code, not placeholders, and the daemon builds as `freesided` (`cmd/freesided`). Per-wave implementation progress lives in the open wave tracker (`Wave N: <Name>`), resolved by the plan's [wave-tracker rule](../docs/plan.md#implementation-coordination-building-freeside-with-agents).

## Claude Subscription Usage Spike

`TestLiveClaudeUsage` tests Claude **2.1.220** with only a setup token. It
runs two separate, bounded invocations: an idle `get_usage` control request
after `initialize`, adding `--input-format stream-json`, and a minimal
inference turn using the writer's `-p --output-format stream-json --verbose`
mode. The idle variant sends no user message. This is test-only evidence for
#1713; synthetic fixtures are not evidence of provider support.

Run the offline evidence checks first, from the repository root:

```sh
go -C daemon test ./internal/ward -run '^TestClaudeUsage' -count=1
```

The live test requires macOS, Apple `container`, its running service, a
cached digest-pinned Claude image in `FREESIDE_WARD_CLAUDE_AGENT_IMAGE`,
`CLAUDE_CODE_OAUTH_TOKEN` supplied through the existing private secret-input
convention. This standalone ephemeral probe starts no Freeside daemon and
uses no GitHub App publication authority. It needs no production rig lease;
leave supervised production and dev instances alone.

```sh
FREESIDE_WARD_LIVE_TEST=1 go -C daemon test ./internal/ward \
  -run '^TestLiveClaudeUsage$' -count=1 -v
```

The test skips unless opted in; missing inputs then fail. It creates fresh
randomly named containers and host-only networks and checks labels and creation
dates before cleanup. A private, non-secret recovery manifest survives failed
cleanup; the test reports its path. Never delete a resource whose ownership
cannot be proved. A networkless seeder populates a named snapshot volume, then
stops before the probe mounts it read-only. The setup token stays on that
snapshot. An empty, root-owned auth file and sticky parent directories prevent
the dropped CLI identity from changing or replacing auth storage while allowing
ordinary config writes. The driver tests both append and unlink rejection.
A fresh environment excludes interactive login, API keys, refresh credentials
and alternate backends. Ordinary scratch is disposable.

A test-only TLS proxy observes method/path counters before forwarding to
the provider. It verifies upstream TLS, blocks refresh requests and blocks
inference during the idle case. The isolated CLI trusts an ephemeral test CA;
no host trust store changes. These are measurement deviations from the writer
launch, alongside the fixed minimal prompt and session ID. The test fails if
its observed topology differs from the declared host-only network and mounts.

Each invocation has a 60-second deadline and a 1-MiB stdout bound. Raw stdout
stays in the probe container and is read through a private bounded pipe; stderr
is discarded. Successful cleanup deletes the snapshot and both containers.
Failed cleanup can retain credentials or raw captures in those private runtime
resources; use the manifest to recover them after proving ownership.
Only reviewed usage fields and verdict metadata enter test output. Unknown
usage fields fail capture instead of being silently discarded. Do not upload
temporary captures or credentials. Review sanitized
examples before committing them. No event in one successful turn means only
`event_not_observed`; auth, transport, timeout and parsing failures mean
`probe_failed`, never `unsupported`.

For private schema diagnosis, `FREESIDE_WARD_USAGE_PRIVATE_CAPTURE_DIR` may
name a new, empty, owner-only directory. It receives bounded raw captures,
including non-usage details, and deliberately survives the run. Keep it
private and delete it after reviewing and extracting only the allowed fields.
It is never a fixture or upload source without that review.

The pinned experiment and collector limits are recorded in
[`devlog/2026-10-02-2245-claude-usage-spike.md`](../devlog/2026-10-02-2245-claude-usage-spike.md).

## Codex Account Probe Spike

`TestLiveCodexAccountProbe` measures Codex **0.147.0** `app-server` startup,
`initialize`, and `account/read`. It uses the production `CODEX_HOME/auth.json`
symlink into a read-only snapshot volume. A host-only network and a test TLS
proxy block every outbound request, counting only `POST /oauth/token` as a
refresh attempt. The auth host alone is not a refresh signal. No provider
request is forwarded. This is evidence for #866, not doctor
integration or a usage observation.

Offline tests need `jq` on `PATH` to exercise the same sanitizer as the image:

```sh
go -C daemon test ./internal/ward -run '^TestCodexAccount' -count=1
```

The live test needs macOS, a running Apple `container` service, and a cached
digest-pinned Codex image with `jq`, named by
`FREESIDE_WARD_CODEX_AGENT_IMAGE`. All three cases use synthetic access-only
stores with made-up claims and an empty refresh token. No real credential or
host auth store is read.

```sh
FREESIDE_WARD_LIVE_TEST=1 go -C daemon test ./internal/ward \
  -run '^TestLiveCodexAccountProbe$' -count=1 -v
```

The `fresh` and `near_expiry` cases send `refreshToken: false` with one hour
and two minutes of token life, respectively; the `control` uses the two-minute
store and sends `refreshToken: true`. The complete test skips unless opted in,
then fails for missing runtime or image inputs. A case selector can run one
case, but the overall verdict requires all three and a proven detector.

Each invocation has a 60-second deadline and a 1-MiB stdout bound. Only fixed
field names, JSON types, the reviewed plan enum, request labels, and measurement
flags leave the container. Unknown fields fail capture; email, account ID,
raw RPC errors, and tokens are never logged. The test checks hashes, symlink
preservation, and append/unlink rejection. `pass`, `fail` (unsafe or unavailable
probe), and `probe_failed` (broken measurement) are distinct; an unproven
refresh detector always leaves the overall result `probe_failed`.
A control pass proves detection only. Transport errors cannot erase a refresh
that was observed, but they always prevent a safe read pass.

Fresh resources have random names and ownership labels. Cleanup verifies
labels and creation dates. A private recovery manifest survives failed
cleanup; retained containers or volumes may contain credentials or raw
responses. Use the manifest to prove ownership before removing them.

## Codex Usage Spike

`TestLiveCodexUsage` measures Codex **0.147.0** `app-server`
`account/rateLimits/read` and the `account/rateLimits/updated` notification
over the same access-only, read-only snapshot. It is evidence for #1714, not
a collector. Unlike the account probe, its proxy forwards: exactly
`GET /backend-api/wham/usage`, and in the turn case
`POST /backend-api/codex/responses`, to `chatgpt.com:443` with provider TLS
verified normally. A query string or an escaped path is not the reviewed
route. Everything else is answered 403 by the proxy itself, nothing is ever
forwarded to `auth.openai.com`, and only `POST /oauth/token` counts as a
refresh attempt.

Offline tests need `jq` on `PATH`:

```sh
go -C daemon test ./internal/ward -run '^TestCodexUsage' -count=1
```

The live test has the account probe's requirements and runs four cases:

| Case | Credential | What It Shows |
| --- | --- | --- |
| `synthetic_fresh` | Made-up store, one hour of life | The read completes with no refresh attempt |
| `synthetic_near_expiry` | Made-up store, two minutes of life | The detector control: the CLI attempts a refresh and the proxy blocks it |
| `real_idle` | Operator's subscription | The provider answers the read; the response's fields |
| `real_turn` | Operator's subscription | The same read, then one minimal inference turn, counting `updated` notifications |

The synthetic cases never reach the provider; the proxy answers their usage
read with a canned body. The real cases need two operator inputs and skip
without the first:

- `FREESIDE_WARD_CODEX_AUTH_STORE`: the path to a subscription `auth.json`,
  an owner-only, singly linked regular file in an owner-only directory. The
  test only reads it, through the production private-file checks, derives the
  access-only snapshot with the production derivation, and compares the
  store's hash before and after. The refresh token never leaves the test
  process.
- Consent to one inference turn. `real_turn` sends a single low-effort
  one-word prompt, which draws on the subscription's allowance. Select
  `real_idle` alone to withhold it.

```sh
FREESIDE_WARD_LIVE_TEST=1 FREESIDE_WARD_CODEX_AUTH_STORE=<path-to-auth.json> \
  go -C daemon test ./internal/ward -run '^TestLiveCodexUsage$' -count=1 -v \
  -timeout 20m
```

A real case launches only when the snapshot's access token has at least seven
minutes left: the CLI's five-minute proactive refresh window, the 60-second
invocation deadline, and 60 seconds for clock skew and launch latency. The
gate runs before any resource exists and again immediately before the driver
starts. A refusal starts no app-server and is reported as `deferred`; a
missing expiry always defers. The driver reports how long after the last gate
check its session began, and a launch slower than the 60-second allowance is
`probe_failed`.

The CLI routinely abandons connections before sending a request. On
`chatgpt.com` that is recorded and ignored, because a lost request there
fails the read or the turn. On `auth.openai.com` the lost request could have
been a refresh, so it counts against the measurement.

Only fixed field names, JSON types, the bucket count, the reviewed plan enum,
request labels, and measurement flags leave the container. Unknown fields and
unreviewed enum values fail capture; usage values, limit names, account
identifiers, RPC error text, and tokens are never logged. Verdicts are `pass`,
`fail` (a refresh attempt outside the control, a lost protection, or a
provider refusal), `probe_failed` (broken or incomplete measurement),
`event_not_observed` (the turn completed without a notification), and
`deferred`. The overall result needs all four cases.

`FREESIDE_WARD_USAGE_PRIVATE_CAPTURE_DIR` optionally names an existing, empty,
owner-only directory that receives each case's raw app-server stream for
diagnosis. With a real credential those files hold account identifiers and
usage values; never commit or paste them. Resource naming, cleanup, and the
recovery manifest follow the account probe. Each case logs its manifest path
before creating anything, because an interrupted run skips cleanup and can
leave a volume holding the access-only snapshot. Until the volume is seeded
the snapshot also sits in a host temporary file; the manifest's
`host_snapshot_copy` entry holds its path, and reads `removed` once seeding
has deleted it.

The measured behavior and the constants later collectors inherit are recorded
in
[`devlog/2026-10-05-1743-codex-usage-spike.md`](../devlog/2026-10-05-1743-codex-usage-spike.md).

## Control Socket and Pairing Codes

Every daemon run owns one private Unix control socket. It publishes the socket
address as `<db>.control.json` beside the database with mode `0600`. Commands
that take `-db` first acquire `<db>.daemon.lock`. If the lock is free, they
hold it while opening the store directly. If the daemon holds it, live commands
use the socket and maintenance commands ask the operator to stop the daemon.
An older daemon that holds the lock without advertising a socket is refused;
the client never opens its live database as a fallback. Requests name the
canonical database path, and both ends check kernel peer credentials.
Live snapshots and Doctor checkpoint scans restrict evidence reconstruction to
the caller's requested recipes that the daemon also approves. Doctor shares the
daemon's checkpoint files and live closure-gap state without changing its
ordinary health policy. Control operations use the caller's
cancellation and deadline, including long-running Doctor scans.

If the startup code expires, run this command as the daemon's OS user:

```sh
freesided pairing-code -state-dir /path/to/existing/daemon-state
```

The daemon must already be running with that `-state-dir`. The command prints
one JSON object with `api_url`, `pairing_code` and `expires_at`. Treat stdout
as private pairing material. The code expires after ten minutes and can enroll
one device; ordinary daemon logs do not contain it. Existing codes retain
their original expiry and redemption state.

The CLI contacts the daemon through its private `pairing-control.json`
advertisement and the same Unix socket. Both ends check kernel peer credentials on
macOS and Linux. The socket lives in a short `0700` temporary directory because
macOS cannot bind Unix sockets under long state paths; the socket and
advertisement are `0600`. A state-directory lock prevents competing daemons
from replacing a live advertisement. Normal shutdown removes only the control
resources it created. A restart uses a fresh socket, without deleting unknown
leftovers from a crash.

Renewal uses the running daemon's existing mint service. It does not open the
database from the CLI, restart the daemon, change work or grant paired devices
host authority. A wrong or unavailable endpoint fails. The network API offers
only the existing pairing preview and redemption, never code minting.

## Testing conventions

**Template store.** Use `storetest.Open(t, path, opts)` from
`internal/store/storetest` for routine file-backed fixtures. It copies a
once-per-process migrated template to a new path, opens it with the supplied
options, and closes it at test cleanup. Each copy seeds its own sync epoch;
an existing path opens as-is so reopen tests preserve their state. Tests of
migrations, `Open` behavior, or refusal of an existing file keep the raw
`store.Open` path. Tests inside `package store` use `openTemplateStore` or
`openTemplateStoreAt` because importing `storetest` would create a cycle.

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
Freeside generates the suggested name, requests the publish permission set
plus `issues: write`, and writes the conversion key directly to the protected
credentials directory. Publication tokens keep their original permissions.

Follow-up filing mints a separate token with only `issues: write` and
`metadata: read` for the one target repository. Its issue calls and App bot
identity reads use that token. An existing installation gains the permission
only after its owner accepts the App's permission change on GitHub. Until
then, filing is refused before dispatch with a `follow_up_filing_refused`
attention item naming the missing permission; publication can continue.
The deployed App's managed installations accepted this change under #1416 on
2026-09-20.

The Claude driver sweeps approved filings at startup and every minute; an
approval also wakes the filer immediately. The fake driver records approvals
but does not file issues. The opt-in `TestLiveFollowUpFilingEffectivelyOnce`
test uses the `FREESIDE_PUBLISH_LIVE_TEST` gate and the
[live-test environment](internal/publish/live_test.go) to file two issues,
recover a dropped create response, and close both issues in cleanup.

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

## Running a Dev Daemon

Agents and scripts run the `ephemeral` tier and nothing else. The
[environment rules](../docs/plan.md#environments-prod-dev-and-ephemeral) are
the authority; this section says which command to reach for.

- **Screenshots: use `-FreesideMock YES`, not a daemon.** The mock needs no
  daemon or pairing, and the `-FreesideSelect` ids name the mock's own
  fixtures, so the same launch gives the same capture every time. See the
  [screenshot recipe](../app/README.md#capturing-screenshots).
- **A live daemon: run `scripts/dev-instance.sh` from the repo root.** It
  builds `freesided` and the Debug app, starts an `ephemeral` daemon in a
  fresh `<worktree>/.dev-instance.XXXXXX` root on `127.0.0.1:0`, seeds the
  `representative` fixture (`--no-seed` starts empty), and launches the app
  with `-FreesideReadinessDir <root>/daemon`. It prints `root=` and `api_url=`
  lines on stdout for a calling script. `--daemon-only` skips the app, for
  scripts and non-Mac hosts. Stopping it (Ctrl-C, or the app or daemon
  exiting) removes the root. A SIGKILL to the script skips that cleanup and
  can leave the daemon and app running; stop them before you delete the
  root by hand.
- **Never run `app/scripts/install-mac-app.sh` from an agent or script.**
  Both of its tiers are the operator's: omitting the tier installs `prod`,
  and the `dev` install is the operator's own too. Its `--prod` flag lets a
  non-interactive caller confirm `prod` on the operator's explicit
  instruction; an agent never passes it on its own.
- **Attaching to production is an operator act.** The `FreesideMacProd` Xcode
  scheme runs a Debug build with `FREESIDE_ENV=prod`, so it connects to the
  `prod` daemon on port `7331`. It is not a passive attachment: the build
  keeps prod's bundle ID and defaults, and its daemon menu is live, not the
  inert demo menu. Launching it can re-register the `prod` LaunchAgent
  against the Debug bundle whenever prod's registration marker is unset, as
  after an install before the installed app relaunches, and its Stop and
  Start act on the `prod` LaunchAgent. Leave it alone unless the operator
  asks.

**Production resources.** Only the operator touches these: through the
`prod` install, or through the operator-only acts above, such as the
`FreesideMacProd` scheme. Agent, script, and `ephemeral` work never does;
this is a rule, not an enforced boundary. The values are the `prod` column
of the plan's derived identifiers table, where `<root>` is the state root:

| Identifier | `prod` |
| --- | --- |
| State root | `~/Library/Application Support/Freeside/` |
| Daemon state directory (`-state-dir`) | `<root>/daemon/` |
| Publication authority state directory (daemon `-publication-state-dir`, onboard `-state-dir`) | `<root>/daemon/` |
| Database (`-db`) | `<root>/daemon/freeside.db` |
| Readiness file | `<root>/daemon/readiness.json` |
| Daemon log | `<root>/daemon/freesided.log` |
| Credentials directory | `<root>/credentials/` |
| launchd label | `ai.freeside.daemon` |
| Bundled LaunchAgent plist | `ai.freeside.daemon.plist` |
| App bundle ID | `ai.freeside.app.macos` |
| Display name | Freeside |
| Listen port | `7331` |

Four more persistent stores sit beside the database, where `<db>` is its
path: the attachment blobs (`<db>.blobs/`), the ntfy device-topic key
(`<db>.ntfy-topic.key`), the local
[encrypted checkpoints](../docs/plan.md#510-coherent-backup-encrypted-checkpoints)
(`<db>.checkpoints/`), and their encryption key
(`<db>.backup-encryption.key`). The daemon's runtime control files are
under [Control Socket and Pairing Codes](#control-socket-and-pairing-codes).

## Operational Commands

`-environment` selects `prod`, `dev`, or `ephemeral`. Without the flag, the
daemon uses `ephemeral` and refuses paths under either supervised state root
and ports `7331` and `7332`. The bundled LaunchAgent plist passes
`-environment prod` or `-environment dev`: the installer fills its
`__FREESIDE_ENVIRONMENT__` placeholder with the tier. See the
[environment rules](../docs/plan.md#environments-prod-dev-and-ephemeral).

`-environment prod` refuses to start, before it opens its database, while a
production rig manifest exists under `~/.freeside/rig-locks` (the account's
passwd home), live or stale: an attended real-work run may be using `prod`'s
App authority. A stale manifest keeps `prod` down until `freesided rig
recover` clears it.

`-prod-app-authority` is that run's one exception to the ephemeral guard
(#1583). It lets `-publication-state-dir` and `-publication-credentials-dir`
name `prod`'s own `<root>/daemon` and `<root>/credentials`, compared by
directory identity, and nothing else under a supervised root. It requires
`-environment ephemeral`; a `-rig-token-file` whose lease is live and
published under that same lease root; `$HOME` and the passwd home naming one
`prod` root; and `-prod-daemon <path>`, the installed `prod` `freesided`
and not this binary. The daemon runs `<path> publication-formats` and refuses
to start unless that build accepts every App authority state format this
build writes. It then holds `prod`'s database lock
(`<root>/daemon/freeside.db`) until it exits, so a running `prod`, even one
started outside launchd, refuses the run, and `prod` can't start during it.
`freesided preflight` holds the same lock while it runs when its publication
directories are `prod`'s, since the run's preflight reads that authority
before the run's daemon starts.
It also refuses when that build writes a version this build can't read.
`freesided publication-formats` prints, per file, the version a build
writes and every version it accepts:
`{"installation_authority":{"writes":1,"accepts":[1]},"installation_janitor_journal":{"writes":1,"accepts":[1]}}`.

The long-running daemon defaults to `-driver disabled`. It serves pairing,
health, stored state, and backups without starting an execution engine or
simulating work. Its inbox explains that agent execution is not configured.
Select `-driver claude` with the required production configuration for real
agents. Select `-driver fake` only for an intentional demo or test; add
`-seed-walking-skeleton` if that demo should start with sample work. Seeding
without an explicit fake driver is rejected.

`-seed-fixture representative` fills an empty `ephemeral` store with
development data, so the app has something to show: two projects, runs in
each lifecycle the task list shows, and one open item for each attention type
except `task_proposal` and `effect_proposal`. It requires `-driver disabled`
and the `ephemeral` tier. It refuses a store that already holds runs or
attention items, and it fails before opening the store for any other tier,
driver, or unknown fixture name. An ephemeral driverless daemon configures the
attended_dev admission floor so the seeded runs keep reading after a restart
without the flag. The fixtures are Go code in `internal/seedfixture`.

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

### Inspect A Live Database

A `prod` or `dev` daemon holds its database in SQLite exclusive locking mode
for as long as it runs, so `sqlite3` and every other process fail with
`database is locked` on the live file. `ephemeral` keeps normal locking.
To inspect a supervised database, copy it first:

```sh
freesided snapshot -db <live-db> <output-path>
sqlite3 <output-path>
```

With a daemon running, the daemon writes the copy over the control socket;
without one, the command takes the daemon lock and copies directly. The output
path must not exist, and the file is created owner-only (`0600`). The copy
holds every row, including device credentials and pairing codes, so delete it
when you are done. While the copy is written, the daemon's other database work
waits for it.

### Configure Client Task Submission

Pass `-manual-submission-config /absolute/manual-submission.json` to the
long-running daemon to enable new tasks from paired clients. The operator
supplies a UTF-8 JSON file with this version-1 shape:

```json
{
  "version": 1,
  "projects": [
    {
      "project_id": "operator-configured-project",
      "policy_keys": [],
      "commit_author": {"app_slug": "configured-app", "bot_user_id": 1}
    }
  ]
}
```

This shows the schema only. Replace the empty policy and example attribution
with approved operator inputs. Each policy key has `key`, `value`, and
`provenance: {"source": "preset" | "override", "digest": "sha256:..."}`.
Supply the complete resolved policy, including the specification settings and
an explicit `paths` boundary. Keys may arrive in any order. The daemon uses
the existing resolved-policy, specification-policy, path, and commit-author
validators; it never creates provenance or chooses defaults for this file.

The daemon loads and validates the whole file once, before opening its command
listener. Files over 4 MiB, invalid UTF-8 or JSON, duplicate JSON members,
unknown fields, unsupported versions, missing or repeated project IDs,
invalid policies, missing or unenforceable paths, and invalid author syntax
fail startup with a `manual submission config` error. A null project list or
entry is invalid. `{"version":1,"projects":[]}` or omission of the flag
disables new client submissions. An unconfigured project returns HTTP 404
without a durable task, run, or command record.

A client supplies only project, source, and optional name. This configuration
is independent of label intake: neither configuration falls back to the other.
Attribution syntax and provenance claims do not grant publication authority.
Production still authenticates the author against the selected GitHub App and
enforces recipe approval, conformance, admission, and specification approval.

Changing the file requires a daemon restart and affects only new submissions.
An explicit manual Retry of a recorded command returns its original result,
even if configuration changed or was removed. A new command always creates
new work, including with identical project, source, and name. For private
staging and retained-session recovery, use the [production walkthrough](../docs/production-walkthrough.md#submit-a-new-task-from-a-client).

### Set A Claude Identity's Parallel Limit

`freesided set-identity-limit` records a newer revision of a Claude writer
identity that changes only `max_parallel_executions`, the number of
executions that identity may run at once. Identities start at 1. The command
works whether `freesided` is stopped or running; a running daemon applies the
new limit at its next admission. It refuses a non-Claude identity and an
identity the store has not recorded, and it prints the previous and new limits
as JSON. Repeating the current limit writes nothing.

```sh
freesided set-identity-limit \
  -db "$FREESIDE_REAL_RUN_STATE_ROOT/freeside.db" \
  -identity "$FREESIDE_REAL_RUN_AUTH_IDENTITY" \
  -max-parallel-executions 4
```

**A limit above 1 does not yet give parallel writer runs.** Every Claude
writer handoff still holds the identity's exclusive auth-store lease for the
whole invocation, so a second concurrent writer on the same identity fails
its stage instead of waiting. Raise the limit only after #1585 lets writer
executions share an identity.

The real-run harness (`scripts/run-real-work.sh`) records the writer identity
at 1 on a fresh state root and never rewrites a recorded identity. After a
raise, watch the daemon log and the paired clients for auth, rate-limit, or
credential failures. On any such failure, set the limit back to the last value
that ran cleanly, or to 1. The
[decision note](../devlog/2026-09-28-1220-identity-limit-rollout.md) records
why this rollout check replaced the provider-overlap experiment.

### Enroll An Agent Credential

`freesided auth add` enrolls one harness client (`codex_cli` or
`claude_code`) for a new or existing auth identity. It records the identity,
binds it to one subscription account, records a client enrollment for the
route, authors the credential store, and appends the enrollment's first store
generation, all under the identity's mutation lease. Stop `freesided` first;
like `enroll-codex`, it refuses a database the daemon holds.

The enrollment id is `<auth-identity>/<client>`, one per identity and client.
Every enrolled identity has a cost owner: name `-cost-owner` when the identity
is created, or when an existing identity still has none. After that, name the
same one or none. The account binding is set once: a second identity for the
same account, or an identity bound to a different account, refuses. A client that already has a store generation refuses too; replacing
an enrolled store is re-enrollment, not `auth add`.

**Codex.** Pass the `enroll-codex` input and store flags (see
[Enroll A Codex Subscription Identity](#enroll-a-codex-subscription-identity))
plus the client, route, and cost owner. The sequence is the one `enroll-codex`
runs: it spends the same refresh token and leaves the same recovery item to
resolve before the identity runs. It reads the account from the login's
`tokens.account_id` and refuses a login without one.

```sh
freesided auth add \
  -db /path/to/freeside.db -client codex_cli \
  -auth-identity codex-primary -route openai-subscription -cost-owner <owner> \
  -project <project-id> \
  -input-root /path/to/codex-enrollment-input \
  -input-file /path/to/codex-enrollment-input/auth.json \
  -auth-store-root /path/to/freeside/review-inputs \
  -auth-store /path/to/freeside/review-inputs/codex-primary.json \
  -approved-recipe sha256:<approved-verify-recipe-digest>
```

**Claude.** Run `claude setup-token` and give the printed token on standard
input, never as an argument: at a terminal the command prompts without echo,
and from a pipe it reads one line. The token must be a setup token
(`sk-ant-oat…`); an API key refuses. `-account` is your attestation of the
subscription account the token belongs to, since the pinned CLI exposes no
account identity to read. The command creates `-auth-volume`, which must not
exist yet, writes the token as the single root-owned `0400` file the
`setup_token` manifest policy requires, and proves that shape with the same
observer preflight runs before it records the generation. Any failure after
the volume is created deletes it; the recorded enrollment stays, and rerunning
the same command retries it.

```sh
claude setup-token   # copy the printed token
freesided auth add \
  -db /path/to/freeside.db -client claude_code \
  -auth-identity claude-main -route anthropic-subscription -cost-owner <owner> \
  -account <subscription-account> -auth-volume freeside-claude-main-auth \
  -exporter-image <digest-pinned-exporter-image>
```

The token is not checked against the provider: `claude auth status` reports
only local state, so a bad or revoked token fails closed at its first use.
A provider probe is tracked in #1647. `auth add` cannot enroll a Claude
identity whose volume already exists; `auth adopt` (below) enrolls an identity
over the volume it already holds. An enrollment runs nothing until an agent in
the admitted-agent tree names it and a lineup line selects that agent.

### Select Agents Through The Lineup

The daemon reads every identity it runs under from the admitted-agent tree
([`policy/README.md`](../policy/README.md)): each ward role's lineup line
selects an agent, and the agent names the enrollment whose identity, cost
owner, and credential store the role uses. `-driver claude` and `preflight`
require the tree as a checkout and an exact commit:

```sh
  -agent-tree /absolute/path/to/checkout \
  -agent-tree-commit <40-character-commit>
```

The tree is read from that commit through git, never from the working tree,
so an uncommitted edit selects nothing. The flags `-auth-identity`,
`-review-auth-identity`, `-review-cost-owner`, and `-shadow-review-cost-owner`
are gone from the daemon and from `preflight`; the model and effort flags stay.

`freesided auth adopt` moves an instance that ran on those flags. Stop the
daemon, then name the identities and cost owners the flags carried:

```sh
freesided auth adopt \
  -db /path/to/freeside.db \
  -auth-identity claude-main -cost-owner <cost-owner> -claude-account <subscription-account> \
  -review-auth-identity codex-primary -review-cost-owner <review-cost-owner> \
  -auth-store-root /path/to/freeside/review-inputs -review-model <review-model> \
  -exporter-image <digest-pinned-exporter-image> \
  -prompt-package <file> -specification-prompt-package <file> \
  -remediation-prompt-package <file> \
  -approved-recipe sha256:<approved-verify-recipe-digest> \
  -patch /path/outside/any/checkout/baseline.patch
```

It enrolls each identity over the store it already holds, writes no
credential, and emits a patch that adds the baseline tree: both agents, a
lineup line for `specifier`, `implementer`, `remediator`, and `reviewer`, and
a `shadow_reviewer` line when `-shadow-review-cost-owner` is given. Review the
patch, commit it in the checkout, and start the daemon with that commit. The
report lists each identity as `adopted`, `reused`, or `unadoptable`; running
the command again changes nothing. The review and shadow review configuration
digests are the ones the flags produced, so existing approvals stand.

The real-run harness runs this command on every start and checks the emitted
tree against the configured commit; its sequence, and where to keep the
checkout, are in
[Enroll The Identities And Check The Agent Tree](../docs/production-walkthrough.md#enroll-the-identities-and-check-the-agent-tree).

#### Name The Cost Owner And The Account

**A cost owner is a label for a bill.** Name it `<scope>-<product>-<letter>`,
for example `personal-claude-a`, `personal-chatgpt-a`, or `team-claude-a`.

- **Keep it opaque.** No name, email, or account identifier. `auth list`
  prints the label in full and an adoption error quotes it, and that output
  gets pasted into issues.
- **Use the letter for a second subscription.** One operator can hold several
  subscriptions to the same product; each is its own bill and gets its own
  label.
- **Choose it once per state root.** No command changes a stored cost owner:
  `auth add` and `auth adopt` accept the stored label or refuse. A fresh state
  root starts without one. The label isn't part of the agent tree, so one
  committed tree serves roots with different labels.

**`-claude-account` is an attestation.** It says which subscription account
the Claude setup token belongs to, and the daemon can't check it. Give the
account's login email.

- **It stays local.** The value is stored in the state database as the
  identity's account binding. `auth list` shows only its last four characters,
  and it enters no tree, API response, or published text.
- **It binds once.** A second identity can't claim the same account, and an
  identity can't be rebound to another.
- **There is no `-codex-account`.** A Codex subscription login names its own
  account, and adoption reads it from the credential store. A Claude setup
  token names none, so the Claude binding is only as good as the attestation:
  a wrong value records that identity's usage against the wrong account.

Adoption enables an identity that was stored disabled, in the same write that
first enrolls it, and reports it `enabled`. A `reused` identity keeps its
stored bit: one reported `disabled` stays disabled, and its lines do not
resolve until `auth enable` (#1639) enables it.

An `unadoptable` identity (its store names no account, or its account is bound
to another identity) gets no enrollment. To retire it, name it:
`-retire-unadoptable <identity>` disables it and records a Stop for each open
task it owns, listed as `stopped_tasks` in the report. The flag is refused for
an identity the same run adopts, so a forgotten argument (for example
`-claude-account`) retires nothing unless the identity is named. Enroll a
replacement with `auth add` and select it in the tree.

A reviewer that authenticates with an API key (`-review-auth-mode api_key`)
cannot be adopted yet: an enrollment has no API-key auth method (#1677). Its
store reports `unadoptable`, the patch carries no `reviewer` line, and an
unattended daemon on that tree stops at startup. Do not retire that identity;
stay on the previous release until #1677 lands, or review with a subscription
login.

At start the daemon checks every role its configuration asks work from:

- **A writer role that fails admission** (no line, a line naming another
  agent digest or prompt, no conformance record, an expired credential) holds
  all admission. The daemon keeps running and raises one `system_health` item
  naming the role. Fix the tree or the enrollment and restart.
- **A reviewer or shadow reviewer line that does not resolve** stops startup,
  because no review source can be composed without its identity. The item is
  recorded before the daemon exits. Each review also rechecks its line first.
- **A retired identity that still owns an open task** (disabled, no
  enrollment, and a task whose newest admission is a flag-era one under it
  has no completed, stopped, or abandoned decision) holds all admission. The
  daemon raises one item naming the identity and the tasks, and stops nothing
  itself: an identity nobody adopted yet looks the same. Adopt it, or retire
  it with `-retire-unadoptable`. The daemon confirms the cancellations after
  its next start; restart it once more to clear the item, which holds
  unattended admission while it is open.

`preflight` resolves the same lines but does not run the writer roles' full
admission check (prompt digest, launch coverage, expiry), because it takes no
prompt package. The daemon's startup check is the authority.

Work admitted before the cutover under an identity that was adopted finishes
under that admission: it is never resolved against the tree.

### List Agent Credentials

`freesided auth list -db /path/to/freeside.db` prints every auth identity with
its enrollments as JSON. It works whether the daemon is stopped or running.
Each identity shows its provider, cost owner, limits, and a `label` that masks
the account binding to its last four characters; each enrollment shows its
client, route, auth method, and current store generation, with a null
generation for an enrollment whose bootstrap never completed. It prints no
token, store content, or full account binding. A stored identity or enrollment
that fails its account-binding re-check refuses the whole listing rather than
showing a credential the store would not create.

### Enroll A Codex Subscription Identity

`freesided enroll-codex` bootstraps a Codex subscription identity and repairs
one whose refresh chain was revoked. It replaces the live auth store only
while holding that identity's mutation lease, immediately spends the supplied
refresh token in a real provider rotation, verifies an access-only agent
snapshot, and binds the resulting store digest and expiry to a recovery
attention item. It never prints or persists token bytes in the database.

Stop `freesided` before running this direct-store maintenance command; it
refuses a database held by the daemon. Create
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

Restart the daemon with `-review-auth-mode subscription`, `-review-input-root`
set to the durable review-input root, and `-review-auth-snapshot` set to the
live store path. The `reviewer` lineup line selects the identity (see
[Select Agents Through The Lineup](#select-agents-through-the-lineup)).
Initial enrollment and recovery both leave the identity blocked until the
operator inspects the displayed digest, fence, and expiry and accepts the
item's `Resolve re-enrollment` action. That command-backed decision, not this
maintenance command alone, clears the revoked-identity marker.

### Renew A Codex Subscription Credential

Use `freesided renew-codex` when an existing subscription store has an expired
access token but still has a refresh token. Stop the daemon first; renewal
refuses its live database. Renewal
holds the identity's mutation lease and uses the same durable refresh
transaction as review launch. It creates no re-enrollment hold and does not
create a task or start execution.

```sh
freesided renew-codex \
  -db /path/to/freeside.db \
  -auth-identity codex-primary \
  -auth-store-root /path/to/freeside/review-inputs \
  -auth-store /path/to/freeside/review-inputs/codex-primary.json \
  -approved-recipe sha256:<approved-verify-recipe-digest>
```

Pass the daemon's approved recipe set, repeating `-approved-recipe` as needed.
Renewal requires the existing database to match the binary's schema and never
migrates it. Use a schema-compatible binary or the supported runtime upgrade
before retrying a schema refusal.
The existing identity must be configured for on-demand refresh and
bound to this exact private store. The command leaves a token with at least
two hours remaining unchanged. Otherwise it refreshes and checks the same
one-hour lifetime floor used by production preflight. Success prints JSON
containing only the identity, store path, digest, expiry, and `rotated` flag.

After interruption, rerun the command: a persisted pending rotation is
recovered without spending the old refresh token again. A revoked chain or
an ambiguous provider result without a recoverable response requires a fresh
`codex login` and `enroll-codex`. An existing re-enrollment hold refuses
renewal. To resolve that hold before production preflight can pass, use
`scripts/run-real-work.sh --recover-codex-credentials` and accept the exact
digest, fence, and expiry in a paired client. See
[Recover Expired Reviewer Credentials](../docs/production-walkthrough.md#recover-expired-reviewer-credentials).
Pass any additional approved recipes to the recovery harness with repeatable
`--approved-recipe` arguments so it can reconstruct retained evidence during
paired-client sync.

`freesided onboard <owner/name>` packages the previously manual path. Stop the
daemon first; onboarding refuses a database it holds. It:

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

If the installation was terminally quarantined, start a fresh native
installation with `-recover-installation <quarantined-id> -installation-id 0`
and the ordinary onboarding arguments. Do not combine recovery with `-resume`
or `-approve`. Recovery retains the prior authority in an owner-only
`installation-recovery-<app-id>-<old-installation-id>-<revision>.json` file,
then replaces only that withdrawn binding and its matching pending request.
The quarantine journal remains unchanged, and the old installation can never
satisfy the new request. Select only the requested repository when installing;
other repositories can be onboarded through the ordinary expansion flow later.
Resume without `-recover-installation` and approve the newly displayed review.
Prior approvals remain historical evidence and do not authorize the replacement.

Private repositories do not need paid branch-protection or ruleset features
to be onboarded. When GitHub explicitly reports those features as unavailable
under the repository plan, the audit retains `plan_unavailable: true` for each
affected feature. All automation-authority reads still run; ordinary permission
errors and incomplete audits still refuse onboarding. Enabling those features
later changes the audit digest and requires the existing profile reapproval.

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

Doctor also reports a `credential_integrity` finding, which is unhealthy when
any enrollment's current generation carries a credential-integrity mark. On a
scheduled pass, and on a one-shot `freesided doctor` that reaches a running
production daemon, a live probe runs first. It checks every enrollment's
current generation under the identity's shared read hold:

- **Truncation, on every store:** a Claude `token` file shorter than the
  setup-token lower bound, or a Codex `auth.json` that is empty or ends before
  its JSON document closes.
- **Corruption, on `external`-refresh stores only:** the stored bytes match
  neither digest convention for the generation's recorded store digest (the
  token-bytes hash `auth add` records, or the whole-volume tree digest
  `auth adopt` records). A store the daemon refreshes in place legitimately
  moves past its recorded digest, so the finding's detail lists it as
  "corruption check not run" and it never gets a corruption mark.

A failed check records the mark, which refuses new admissions on that
generation until the enrollment is re-enrolled. Only a finished observation
marks, and a corruption finding marks only when a second observation under the
same hold reports the same digests. One that does not reproduce records no
mark and is listed in the detail as "corruption finding not reproduced"; a
truncation both observations report still marks. A store the probe cannot
observe (a missing volume or file, a runtime failure, a live mutation lease, a
generation that changed during the observation, two observations that disagree
on the token's length) is listed in the detail as not checked, with a fixed
reason code, and the pass continues; the cause goes to the daemon log. The
probe takes no mutation lease, triggers no refresh, and writes to no store.

Unlike the other findings, a marked generation files an `advisory`
`system_health` item (diagnostic code `credential_integrity`, impairing
`agent_credential`), one per marked generation, naming the identity by its
masked label. A later pass never refiles an item the operator handled, and
resolves an open one once the enrollment's current generation has moved past
the marked one. The startup pass and a one-shot doctor with no daemon running
start no observer: they report the marks earlier passes recorded and say so in
the detail. The probe is not the account probe: it reads stored bytes only and
never contacts a provider.

The [production walkthrough runbook](../docs/production-walkthrough.md) covers
the harness's retained endpoint, explicit completion, interrupted-holder
recovery and verified restoration of the supervised daemon. Final verification
reads the existing database beside the running daemon; it never seeds identities
or migrates state. Keep the foreground harness open through the client walkthrough.

Each ordinary `freesided submit` invocation creates a new task and campaign,
even with identical inputs. Before acceptance, it saves a submission identity
and the exact input bytes in `<db>.submissions/<identity>.json`, then prints the
identity and retry command to stderr. After an interrupted or uncertain result,
use `freesided submit --db <path> --retry-submission-id <identity>` to retry that
saved submission. Retry needs no original input files and never rereads them.
The journal contains the submitted task and configuration; keep it with the
database and its access controls.

For a preflight-bound submission, choose one `--submission-id <identity>` and
pass it to both `preflight` and `submit`. The real-work harness saves this identity
before preflight. Reusing it with different input values is refused. Legacy
`--run-id` is lookup-only: it may recover an existing historical run but cannot
create work. Attaching to a retained real-work session observes its existing
identity without submitting again.

`freesided preflight` is the pre-start production-composition gate used by
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

Following reads the daemon's own durable observation projection through the
control socket when it is running and through a locked direct store when it is
stopped, the same routing rule as `freesided submit`. It never reads a live
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

### Rebuild A Project Image For A Dependency Change

A candidate that changes `package.json` or `package-lock.json` cannot verify
in the image onboarding approved, because that image bakes the old
dependencies. Production driver mode rebuilds the image itself when the change
stays inside the project's declared policy (`docs/plan.md` §5.7,
**Policy-gated rebuild**), and blocks publication otherwise. The gate runs in
the publication lane immediately before verification, on first publication
and on every re-entered cycle. A head that keeps the manifests the image
baked takes no gate.

Three flags give the daemon the host-specific build inputs no image record
carries:

| Flag | Meaning |
| --- | --- |
| `-base-build-ref <tag>` | Local tag the container runtime resolves to the approved agent base, as for `freesided onboard`. Enables the rebuild. The build proves the tag's digest equals the admitted image's base before using it. |
| `-build-proxy <url>` | Build-only HTTP proxy without credentials, overriding the managed build proxy. Needs `-base-build-ref`. |
| `-build-dns <server>` | Build DNS server; repeatable. Needs `-base-build-ref`. |

Everything else carries over from the admitted image's record: the
repository, the recipe, the base image, and the registry destination. The
rebuilt image is pushed beside the admitted one under the tag
`rebuild-<first 12 hex of the commit>`, so the daemon's host needs the
registry access onboarding had. The build reads the candidate from the lane's
own checkout at the verified commit and makes no forge request.

The gate holds when the candidate, compared with its base:

- Holds neither `npm-shrinkwrap.json` nor `.npmrc`.
- Declares the same verification recipe.
- Runs under a policy that declares a valid `registry_set`.
- Leaves `package.json`'s `overrides`, `workspaces`, and `packageManager` as
  they are.
- Declares every dependency it adds or respecifies in `package.json` by a
  registry version, range, or tag.
- Has an npm v2 or v3 lockfile whose root agrees with `package.json`, in which
  every package entry added or changed is a registry tarball: an exact
  version, an `https` URL on a declared host, and a `sha512` integrity value.
- Holds a lockfile entry for every dependency of the root and of each added
  or changed package, with no peer dependency nested under the package that
  declares it.

The gate reads the candidate's files, which npm reads again inside the build
with network access, so it refuses whatever it does not recognize in what the
candidate changed. A changed lockfile entry is refused when its key is not a
plain `node_modules/<name>` path, when it carries a field outside the gate's
list, when it is a link, a bundled package, or a package with its own
shrinkwrap, or when it depends on anything but a registry version, range, or
tag. `npm:` aliases are refused with the URL, git, and path sources. An entry
the base lockfile holds byte for byte is not re-examined, and neither is a
`package.json` dependency the base declares identically: both are in the image
a person approved.

> **The gate reads manifests; it does not bound the install's connections.**
> npm keeps a lockfile entry only while every dependency that reaches it is
> satisfied by it. Otherwise it asks the default registry for the package and
> follows that registry's metadata, whatever the lockfile pins. The gate
> refuses the cases it can read without npm's own version arithmetic (a
> dependency declared by URL, a dependency with no entry, a nested peer). It
> does not check that a locked version satisfies the range that reaches it. A
> candidate built that way can make the build contact a host outside the
> registry set, and the build proxy admits any public host. Until the build's
> egress is held to the registry set (#1793), give a daemon `-base-build-ref`
> only where that is acceptable.

Two ordinary shapes are refused and need a person to rebuild: a package that
depends on another through an `npm:` alias (`glob` 10 does, through
`@isaacs/cliui`), and a registry on a host npm reads as a git forge
(`github.com`, `gitlab.com`, `bitbucket.org`, `git.sr.ht`). So is a rarer
one: a manifest in which one object holds two keys that differ only by case,
such as a tree with both `JSONStream` and `jsonstream`.

The rebuild then runs the builder's own proofs (the networkless positive run
and the masked-cache probe), records the image, and binds the run to it.
Verification runs in the rebuilt image, still with `--network none`, and the
verification checkpoint names that image. The admitted image still answers
whether the run's base is one it serves. A rebuild leaves the admitted image
in place for later runs: a recorded rebuilt image is reused only by a
candidate whose manifests it baked.

A refused gate names one clause:

| Clause | The candidate, or the daemon |
| --- | --- |
| `undeclared_authority` | Resolves a changed package from a host outside the registry set. |
| `unpinned_source` | Declares or pins a changed package as something other than a registry package at an exact version, from an `https` tarball with a `sha512` integrity value: a link, an alias, a VCS, URL, or file source, a bundled or shrinkwrapped package, a missing pin, a lockfile field the gate does not know. |
| `recipe_changed` | Changes the verification recipe along with its dependencies. |
| `lockfile_inconsistent` | Has a lockfile that is unreadable, repeats a key or holds two that differ only by case, is not v2 or v3, disagrees with `package.json`, lacks an entry for a dependency, or nests a peer dependency under its dependent; or its base's manifests cannot be read to compare with. |
| `unsupported_npm_input` | Holds `npm-shrinkwrap.json` or `.npmrc`, or changes `package.json`'s `overrides`, `workspaces`, or `packageManager`. |
| `no_registry_set` | Runs under a policy with no registry set, or a malformed one. |
| `rebuild_not_configured` | Passed the gate, but the daemon has no `-base-build-ref`. |
| `build_or_proof_failed` | Passed the gate, but the image failed its build-time proof, or the build did not finish within one hour. |

No verification room runs for a refused gate. Where the clause appears
depends on the cycle:

- **First publication:** The run blocks on the existing `publish_blocked`
  item for a failed verification. Its one evidence artifact begins
  `Project image rebuild refused: <clause>.` and says what failed the clause.
  The item's reason text is the shared verification-failure reason, because a
  per-clause reason is a contract change this lane does not make.
- **Re-entered cycle:** The cycle stops, and the stop item's reason names the
  clause and the commit.

A build fault that is not a proof failure (the registry unreachable, the
container runtime down) is the build's environment, not a verdict: the task
stays queued under the lane's visible `publication_environment` hold and the
next paced attempt builds again. That includes an install the candidate's own
lockfile breaks, such as a tarball that fails its integrity value: the builder
does not tell that apart from a registry fault, so it retries on every paced
attempt. A restart during a build converges on the image the build recorded
and does not build a second one.

One build, with its proofs, is bounded at one hour. The proofs run the
candidate's verification commands, and the lane handles one task at a time, so
a build that does not finish is a verdict (`build_or_proof_failed`) and not a
retry. The bound covers the build and its proofs, not the builder's cleanup
afterwards: a container runtime that stalls while images are removed holds
the lane until the daemon restarts (#1806).

A refusal is the task's recorded verification result, so it does not clear
when the daemon is later given `-base-build-ref` or the policy changes. It
reruns as any failed verification does: after a revised trust profile,
through the item's rerun action, or in a new run.

## External Review Replies

With the Claude driver, the daemon checks recorded external-review outcomes
at startup and once a minute. It replies only while the finding's reviewer
is admitted by the repository's active trust profile. Inline findings receive
a reply in their original thread. Findings on a review body receive a pull
request conversation comment linking that review.

Declined and deferred replies include the screened reason. Fixed replies wait
until a head reviewed in the remediation round or later has been published.
They name the published commit and say that Freeside has not proven the finding
fixed. The fake driver posts no replies.

The following advisory, acknowledge-only health items describe reply failures:

- `external_review_reply_refused`: the reason failed screening, the thread
  could not receive a reply, the App identity changed, or GitHub rejected the
  request. The reply will not be retried.
- `external_review_reply_ambiguous`: the request may have posted, but recovery
  could not identify exactly one new comment from the recorded App account.
  Check the pull request by hand. Acknowledging the item does not resend it.
- `external_review_reply_unreadable`: stored dispositions could not be
  reconstructed. Replies for that run wait until the records can be read.

An uncertain send is never repeated. Recovery waits ten minutes before looking
for its comment; a restart begins that wait again. Only a proven rate-limit
rejection allows retry, at most three attempts per daemon process and at least
one minute apart. These notices do not block publication or review.

## Run Observation Contract

The run-monitoring contract (issue #394; plan §8) lets an operator client
follow an unattended run from submission through admission or hold,
invocation start, terminal collection and import, and final outcome. The
model lives in `internal/domain/observation.go`; the read surface is
`store.ReadTx.ObserveRun`, consumed through the control socket while the daemon
is running and directly under the daemon lock while it is stopped. The
timeline is persisted, so reconnect and daemon restart preserve it. Its
first consumer is `freesided follow` (issue #409), whose display lives in
`internal/observe`. That package is the whole verb, and its imports are held
to a closed allowlist that names no way to open a file, start a process, or
open a socket, so the containment below is structural rather than a promise;
the `cmd/freesided` file is a shim supplying streams, interrupt, and exit
code. The database is reached only through `internal/observe/observedb`,
whose exported surface is a direct or borrowed opener, bounded reads, and close, so no
write, checkpoint, restore, or backup-file capability is in the follow path
at all.

- **Milestones** are an append-only, first-observation-wins timeline of
  typed events (`run_submitted` through `publication_ready` or
  `publication_blocked`, and `work_unit_completed` once the published work
  unit's completion criterion is satisfied), written inside the transactions
  that commit the underlying workflow facts. `work_unit_completed` carries
  only the publication invocation; the PR, merge commit, and bound issue are
  read from the re-gated `work_unit_completions` row, and the sync boundary
  fails a run closed when the milestone has no supported row behind it.
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

### Subscription-Backed Daemon Judgments

The production classifier, finding adjudicator, drift auditor, and task namer
can use the existing Claude subscription setup-token mechanism. The drift
auditor (`drift_auditor`, plan §7 Review Drift) runs once per review round
from the round `review.drift_audit_after` names, on a round that reported
findings and that no deterministic stop cause already parked; with the key
unset it never runs. A failed audit is recorded once as a
`drift-audit-failure-<round>-<run>` evidence artifact and the round continues
as if the audit were off. A `converged` verdict changes nothing. A `stuck`
verdict parks the round on its `review_diminishing_returns` item with the
`drift_audit` cause. The run's first `over_hardened` verdict that passes the
store's route gate becomes the round's remediation under
`review.drift_audit_route: auto`: the remediation input carries the audit's
reversal list with engine-derived paths, and the same write records one
disposition-supersession record per reversed fix. Every other `over_hardened`
verdict parks, and the item says whether `continue_under_policy` would run
the simplification round. The drift replay
(`internal/integration/drift_replay_test.go`) re-runs recorded audits of real
over-hardened pull requests through this site and the route gate, and
`FREESIDE_DRIFT_REPLAY_LIVE_TEST=1` audits them again with the live provider.
Configure all four flags together:

```text
-judgment-claude-bin /absolute/path/to/claude
-judgment-claude-sha256 <sha256-of-that-native-executable>
-judgment-model <explicit-Claude-model>
-judgment-auth-snapshot <existing-token-file-relative-to-review-input-root>
```

Use the existing private setup-token snapshot, with the same ownership and
permissions required for Claude shadow review. No new API key or login is
needed. The default, with all four flags absent, keeps inference unavailable
and preserves the existing conservative fallback behavior. The task namer keeps
the identifier name when judgments are unbound or naming fails. Naming uses a
separate serial worker after durable dispatch, so a slow provider does not
delay reconciliation. Its 32-entry in-memory queue keeps the identifier
fallback when full or lost on restart. Diagnostic and
Discussion sites continue to use their fallback outputs with this adapter.

One further optional flag configures the publication-author role's two sites
(`publication_author_explain` and `publication_author_propose`):

```text
-judgment-publication-author-prompt <path-to-publication-author-role-prompt>
```

It names the refinable role prompt file (`prompts/publication-author.md`). The
daemon reads it at startup, folds the file's content digest into the judgment
configuration digest preflight records, and hands the bytes to the Claude
judgment driver, so a different prompt file is a different, preflight-checked
configuration. With the flag unset, both publication-author sites return their
fail-safe (empty prose and "no close") and nothing blocks. The prompt file must
be non-empty and at most 32 KiB.

The adapter pins and privately copies the native CLI, supplies allowlisted
fields through stdin, and launches with safe mode, no tools or MCP, no saved
session, and an empty temporary home/configuration directory. It does not give
the model a repository workspace. It rejects incomplete responses, missing or
contradictory usage, model substitution, and invalid judgment JSON. Calls have
one turn, the site's deadline and output-token limit, and one shared in-flight
slot across judgment sites. Compute units mean generated tokens; input bytes
have their own site limit. Classifier, adjudicator, and drift auditor calls
can cancel an active task namer, then wait within their own deadline for its process and
private-directory cleanup before taking the slot. Naming never preempts
another call; overlapping workflow judgments still fail immediately. The
existing ledger reserves each call's full allowance before dispatch.
Account-lineup attribution remains the separate
#900 design decision; this binding does not represent it as implemented.

That no-tools launch is the safety argument for running a judgment call on the
host, and its proof is interim. Plan §5.4 (Admission) makes the call launch a
capability the adapter build's conformance record proves in the stage contract
suite (#1424). Until that lands, the proof is a hand audit of one Claude CLI
build: the executable `-judgment-claude-bin` names, pinned by
`-judgment-claude-sha256`. The recorded basis is the localhost
synthetic-provider probe of Claude CLI 2.1.267 on 2026-09-09
(`devlog/2026-09-09-2145-subscription-judgments.md`), which observed one model
request with an empty tool list. The audit covers that build and no other, so
a different pin runs unaudited until it is audited again. The deterministic
fake driver and the site budgets do not stand in for it: they cover output
handling, not what the harness can do. No second call driver joins on a hand
audit. The wardless admission class carries this as `InterimCallLaunchAudit`
(`daemon/internal/domain/wardless_admission.go`): it admits a `claude_code`
adapter only under an audit record naming that adapter's digest and harness
build. The record compares the adapter's authored harness build, not the
executable's SHA-256, and which build was audited is deployment configuration
the caller supplies. No judgment site reads the lineup or that admission yet;
#1425 wires both.

Preflight reports `judgment_configuration` and a secret-free
`judgment_configuration_digest`. The real-run harness accepts the corresponding
`FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN`,
`FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256`,
`FREESIDE_REAL_RUN_JUDGMENT_MODEL`, and
`FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT` environment variables. Retained
restoration requires the same binding, including its credential content;
adding a judgment backend to an unbound retained session is a configuration
change and is refused. A separately configured challenge can enable it.
At startup, enabled judgments also require `-judgment-configuration-digest`
from successful preflight. The harness supplies it from the immutable manifest;
a token rotation or configuration change between preflight and startup is refused.

Before deploying a different native CLI pin, run the protocol probe from
`daemon/` with `FREESIDE_JUDGMENT_CLI` and
`FREESIDE_JUDGMENT_CLI_SHA256` set:

```sh
go test ./internal/claudeinference -run TestPinnedClaudeCLIProtocol -count=1 -v
```

It uses a synthetic token and a localhost mock provider to check the actual
CLI's model, zero-tool input, output bound, and terminal response format.
It does not perform a real provider call or establish live review acceptance.

## Task Cancellation Contract

`stop_task` on `/commands` accepts a paired device's task ID, project ID,
expected sync epoch, and observed task snapshot version. It persists an
immutable receipt and task fence. The task's `cancellation` field is null or
carries `requested`, `failed_to_stop`, or `confirmed` independently of lifecycle
and WIP. Exact command replay returns the original receipt and revision.
A new command is accepted when its version is between 1 and the current
revision, so unrelated revision movement no longer rejects it; a stale epoch
or a greater version returns the current task snapshot and epoch.

The daemon discovers Stop requests on a separate reconcile loop. It cancels
registered task work, fences new invocations and publication, and asks concrete
stage, review, verification, and judgment adapters to prove quiescence. An
attempt has a two-minute wait bound; a timeout records `failed_to_stop` and
retains WIP. Retries never turn a missing local session into proof of exit.
Specification question and approval cards route Stop through the same fence;
#1369 owns task-level client controls.

The native Claude judgment adapter reports quiescence only after joining its
CLI and observing the exact owned process group absent, or proving no launch.
The task journal sits below the inference client's timeout and retains that
proof independently of whether the inference succeeded. Tasks predating the
ownership checkpoint, restored database epochs, interrupted host commands,
and unresolved publication effects retain WIP until proof is available. See the
[controlled stop check](../docs/task-cancellation-check.md) for the evidence
needed to distinguish an accepted Stop from confirmed termination.

A bound confirmed acknowledgement atomically releases
any held episode and projects `stopped`; an already completed episode stays
`finished`. Explicit abandonment projects `abandoned` without claiming that
execution stopped. Stopped and abandoned tasks leave Active filters and remain
in Finished/All history. A confirmed queued task needs no run or position.
Ordinary submission cannot reopen a terminal episode; an uncancelled explicit
retry needs atomic cap-checked admission, and cancellation forbids it. No client may set
cancellation state. The mock also stays pending unless a test explicitly seeds
acknowledgement evidence. See [the plan](../docs/plan.md#59-durability-effectively-once)
for admission/publication ordering, late results, and restart reconciliation.
