# Classify A Rejected Reviewer Credential As Configuration

Work unit: #2110. Mandatory note: returned-object trust boundary. Codex's
event stream, and now in one case its stderr, decides whether a failed
review parks for the owner or spends a retry.

This revises `devlog/2026-08-11-1908-codex-terminal-event-classification.md`
in two bounded ways. That note made the last decoded `turn.failed` the only
authority and let a missing or unrecognized terminal message spend transient
retries, and asked to be revisited when a captured failure proved otherwise.
The Wave 8 exit run (#1613) is that capture: the provider rejected the
reviewer's access token, and run `run-e0014ee7…` spent rounds 6 to 15, about
an hour, retrying it as `transient`.

## Decisions

- **A blocked refresh is a rejected credential.** Chose to read a
  `turn.failed` that names the refresh endpoint (`auth.openai.com/oauth/token`,
  derived from `codexAuthRefreshEndpoint`) or says the token "could not be
  refreshed" as an in-container refresh attempt, classified `configuration`.
  The review profile forbids the refresh endpoint, so Codex only reaches for
  it when the provider has rejected the access token the daemon supplied.
  Every recorded round ended this way. The 401 itself appeared only on stderr.
- **Stderr decides one narrow case.** Chose to let a stderr line carrying
  Codex's own 401 markers (`401 Unauthorized`, `auth error: 401`) classify
  `configuration` only when the exit status is nonzero and no terminal event
  decoded after the last `turn.started`. The owner reproduced Codex exiting
  on the rejected token without `turn.failed` (issue comment, 2026-10-21), and
  stderr is then the only record of why. Rejected scanning stderr whenever a
  failure is unrecognized: a decoded terminal event stays the authority, so a
  recovered 401 before `turn.completed` or a later `connection reset` keeps
  its old class. Only lines that are not JSON envelopes count, so model and
  tool text inside an event never does.
- **"usage limit" is quota.** Codex's usage-limit message matched none of the
  quota terms. The term joins the set shared with the Claude provider.
- **The provider's message travels with the failure.** The outcome's failure
  now reads "Codex review exited with status N: <message>" (or "attempted an
  in-container credential refresh: <message>"), bounded by
  `exec.TruncateSummary`. It reaches `review_failures.reason` and the parked
  item's reason, so `freesided inspect task` shows the cause without a read
  of `codex_review_outcomes`. The synthesized "turn did not complete" message
  was never shown, so it became empty: a started turn with no decoded message
  keeps the old text.
- **No new failure class.** `configuration` already parks the run under the
  reviewer-configuration item, so nothing in a shared package changes. Its
  wording ("a reviewer configuration the trust profile no longer approves")
  fits a rejected credential loosely; changing it is engine work and stays
  out of this unit.

## Refute-First Verification

An independent fresh-context pass ran each attack through
`normalizeCollection` and `classifyCodexTerminalFailure` and found no
defect.

- **Disproved:** 401 stderr lines followed by `turn.completed` (a pass at
  exit 0, `transient` at exit 1) or by a decoded `turn.failed` "connection
  reset" (`transient`); "usage limit" or the refresh endpoint inside
  `item.completed` or a top-level `error` event (`transient`, since only the
  `turn.failed` message is read); 401 lines only before the last
  `turn.started` (`transient`); a truncated JSON line holding "401
  Unauthorized" (skipped, it starts with `{`); invalid UTF-8 and a 900 KB
  stderr line (the failure stays valid UTF-8 and bounded). The two turn
  readers share `codexTurnEvent`, so they cannot disagree about whether a
  turn is open. Removing the "turn did not complete" text changes no class
  or recorded failure.
- **Accepted:** a JSON line that is not an object (`["401 Unauthorized"]`)
  counts as stderr. Codex writes only objects to stdout, so the case is
  unreachable.
- **Accepted:** a websocket 401 that recovers over HTTPS, followed by a
  nonzero exit with no terminal event, parks under `configuration` though the
  401 was not the cause. Model or repository text cannot reach raw stderr:
  tool output arrives inside JSON items, and `--ignore-user-config` with
  `project_doc_max_bytes=0` keeps the repository from adding MCP servers.
  Every false case needs a nonzero exit with no terminal event, and its worst
  outcome is a run parked for the owner with the stderr line visible, never a
  pass or a skipped gate.
- **Accepted:** exit 78 now appends the terminal message to the sandbox
  text. The access command exits 78 before Codex starts a turn, so no 401
  line can follow it.
- **Accepted, intended:** the Claude source shares `normalizeCollection` and
  the shared term sets, so its failure now carries its `is_error` result
  text, and a Claude "usage limit" result is `quota` instead of `transient`.
  Nothing outside `ward` parses the failure text.
- **Fixed:** the equivalence harness still said it "calls the unchanged
  package helpers"; it now names the helpers #2110 revised and says the
  comparison covers composition, not their classification.

## Revisit When

A Codex upgrade changes its tracing text: the stderr fallback depends on it
(`client_version=0.146.0` in the fixture), and a change silently returns the
stderr-only shape to `transient`. The structured refresh path does not.
Also revisit if Codex starts a proactive refresh for a token the daemon still
considers fresh; that would park healthy runs under `configuration`.
