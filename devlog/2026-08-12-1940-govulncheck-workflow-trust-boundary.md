# govulncheck Workflow: Trust-Boundary Dispositions

Work unit: #736 item 1 (PR #738). Mandatory note: a privileged
(`issues: write`) CI workflow that checks out repository code and
consumes externally returned data (govulncheck JSON, the GitHub issue
list) is credential-leak and returned-object trust-boundary work under
the high-assurance profile. Scope: `.github/workflows/`, `devlog/`.

## Decision

Chose a single scan job holding both `checkout` and `issues: write`,
over the two-job no-checkout observer split used by
`daemon-race-incident.yml`. The split exists to keep *untrusted upstream*
code away from the issue-write token; that motivation is absent here
because the job runs only on trusted refs. The boundary is the trigger
list: `push` is main and `schedule` runs the default branch, both of
which supply the workflow definition and code from the default branch,
which an untrusted ref cannot control. There is no `pull_request`
trigger, so PR-authored code never reaches this job, and no
`workflow_dispatch` trigger, because a dispatch runs the *selected*
ref's workflow and an in-file guard cannot constrain the ref that
supplies it (see the dispatch disposition below). The job-level
`if: github.ref == 'refs/heads/main'` is retained only as defense in
depth for any future trigger, not as the boundary.

Chose not PR-blocking (main + schedule, no PR trigger): a newly
published advisory against an unchanged dependency must not redden
unrelated open PRs. Owner decision, recorded on #736.

## Refute-First Dispositions

Independent lens (Codex, refute-first) over the privilege/trust surface.

Confirmed and fixed:

- **Fail-open scan (P1).** The completion gate keyed only on the leading
  `config` frame, which govulncheck emits *before* analysis, so a scan
  that emitted config then died (package load, DB fetch) was reported as
  a clean pass. Verified against govulncheck v1.6.0 source
  (`internal/scan/errors.go`, `text.go`): the code-3 "vulnerabilities
  found" status is text-mode only; `-format json` exits 0 on any
  completed scan and non-zero only on operational failure. Gate now
  fails on `gv_exit != 0` as well as the config-frame check.
- **Privileged dispatch from an untrusted ref (P1).** The `issues: write`
  job could run an arbitrary dispatched ref's workflow definition and
  analyzed code with the token. A first fix added a main-only `if` guard,
  but that guard lives in the dispatched workflow itself, so a ref could
  edit the guard away while keeping the token: an in-file guard cannot
  constrain the ref that supplies it. Fixed completely by removing the
  `workflow_dispatch` trigger, leaving only push-to-main and schedule
  (both default-branch), so no ref-selected workflow ever runs with the
  token; the `if` guard stays as defense in depth for any future trigger.
  The earlier "trusted refs only" comment, wrong about `workflow_dispatch`,
  was corrected.
- **Stale recurring-vulnerability info (P2).** A still-open finding was
  skipped without refreshing its issue, so a later-published fixed
  version or changed affected-module version left stale remediation
  guidance. Fixed by delimiting the mutable advisory facts between
  `govulncheck-details` markers and editing the canonical issue in place
  only when those facts change (verified round-trip: a rebuilt body's
  details equal the extracted stored details, so an unchanged advisory
  triggers no edit and no daily churn). The refresh splices only the
  marker-bounded region into the existing body, so operator-authored
  notes or checklists outside it survive the edit.
- **Destructive splice on malformed markers (P2).** The first splice
  implementation gated only on the presence of the start marker, so a
  body with a start but no end marker (an operator who deleted the end
  marker) would splice to EOF and silently drop every operator line
  below the start. Fixed by requiring exactly one ordered start/end pair
  before editing; any other shape (missing, duplicated, or reordered)
  fails closed, leaving the body untouched and emitting a warning for
  manual refresh, so no `gh issue edit` can destroy operator text.
- **Incomplete returned-object validation (P2).** `validate_issue_list`
  originally checked only `.number`, so an issue entry whose `.body` was
  neither a string nor null would pass. The optional `match(...)?` then
  silently treated that malformed body as marker-less, so a covered
  vulnerability looked uncovered and got a fresh duplicate issue every
  scan instead of failing closed at the boundary. Fixed to also require
  `.body` be a string or null, rejecting any other type before an issue
  write (verified: string/null/absent bodies accepted, object bodies and
  non-numeric `.number` rejected).
- **Fail-open on malformed scan output (P2).** The vulnerability-ID
  extraction ran `jq` inside `mapfile < <(...)`, which does not propagate
  the process-substitution exit status, so a `jq` failure on an otherwise
  valid (config-frame-present, zero-exit) stream would leave `vuln_ids`
  empty and report a clean scan. Fixed by asserting each finding's shape
  (string non-empty `osv`, array `trace`) in `jq`, so a changed
  govulncheck schema, or an empty id that would slip through the
  blank-skipping loop as a false clean, errors rather than silently
  yielding zero IDs, and by capturing the result in a status-checked
  command substitution that fails the job on any `jq` error.
- **Single-platform coverage gap (P1).** The scan ran only on the Linux
  runner, and govulncheck source mode analyzes only the current
  GOOS/GOARCH build graph, so a vulnerability reachable solely through
  Darwin-tagged code (`internal/atomicfile` calls `unix.RenameatxNp` on
  Darwin, `unix.Renameat2` on Linux) would report a false clean scan for
  the supported Mac deployment. Fixed by scanning both `linux/amd64` and
  `darwin/arm64` (matching daemon-ci.yml's matrix) with a host-native
  govulncheck binary and per-target `GOOS`/`GOARCH`, unioning the
  results; verified the two targets compile genuinely different files and
  each completes independently.

Accepted by decision (not defects):

- **OSV DB content treated as non-hostile.** Advisory summary, module,
  and symbol strings from `vuln.go.dev` are interpolated into issue
  bodies. Injection is structurally prevented (`--body-file`, jq field
  access, never `eval` or a shell-expanded template), and the Go vuln DB
  is a curated first-party source. Fields are not otherwise trusted for
  control flow.
- **Coverage decided by marker match, not free-text parse.** Once the
  issue list is validated, the marker regex and `contains` match, not
  prose parsing, decide which vulnerability an issue covers.

Rejected by verification: none.

Revisit when the govulncheck JSON schema changes (the `config`-frame
completion signal, the `finding.trace[].function` "called" test, or the
`osv`/`finding` shapes the detail builder reads); when the daemon gains a
supported platform beyond `linux/amd64` and `darwin/arm64` (add it to the
scan matrix, kept in step with daemon-ci.yml); or if the Go vuln DB stops
being a trusted source (then advisory text needs sanitizing before it
reaches an issue body).
