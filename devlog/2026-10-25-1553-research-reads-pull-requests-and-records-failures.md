# Let Research Read Own-Repo Pull Requests and Hand Back Failed Fetches (#2311)

Work unit: #2311. Scope: `daemon/internal/specify`, `daemon/internal/engine`,
`daemon/internal/publish`, `prompts/phase-1a/specifier.md`,
`daemon/README.md`, and `devlog/`. This widens a credential's grant and the
endpoints it reaches, so the note is mandatory. It extends
`devlog/2026-10-23-1007-authenticated-source-issue-research.md` (#748).

## Problem

The Wave 8 exit run's line-4 rehearsal (#1613) ran the specifier on a private
fixture repository. It asked for the issue, its comments, and the earlier
pull request that settled the contract it extends. The fetcher authenticated
only issue endpoints, so `pulls/1` went out bare and 404'd, and that one
failed fetch ended the run with an `execution_failure` item. The specifier
never saw why.

## Decisions

**Widen the research grant to `pull_requests: read` without an App change.**
The App registration already declares `pull_requests: write`, which
publication uses, so every working installation grants it. A read mint
narrows that grant the way the `issues: read` mint narrows `issues: write`.
The exact-grant check still fails closed on any other answer. The names
`IssueReadPermissions`, `IssueReadTokenSource`, and
`NewFetcherWithIssueTokens` stay; renaming them would pull
`daemon/cmd/freesided` into scope for no behavioral gain.

**Authenticate `pulls/N` and its sub-resources under the issue rules, and
nothing broader.** The path must be `issues` or `pulls`, then a positive
decimal, rewritten to the repository-ID endpoint, with the same plain-segment
and plain-query rules. The `pulls` listing, `pulls/0`, and `pull/N` stay
unauthenticated. A listing could have been useful research, but it widens
what an untrusted writer's request can enumerate with the credential, and no
observed run needed it.

**Hand a failed fetch back to the specifier, over failing the run.** A
specifier's request is a guess about where evidence lives; a 404 or an
outage is an answer it can use (work from other evidence, ask a decision, or
retry a transient failure). `FetchOrRecordFailure` stores a non-2xx status
after the credential retry, or a transport error, as a research artifact
with the status (0 when no response arrived), no body or content type, and a
daemon-written `failure` text. It is replayed like any research artifact, so
a crash after the write never refetches. `max_iterations` still bounds a
specifier that keeps asking.

**Keep policy and limit failures fatal.** An allowlist refusal (including a
redirect off the allowlist), a host that resolves to a non-public address,
an oversized response or envelope, a non-UTF-8 body, a missing final URL,
cancellation, and store errors still fail as before. These say the request
or the daemon is wrong, not that the evidence is absent; recording them
would teach the specifier to probe the policy. The non-public dial error now
wraps `ErrResearchURLRefused` so the recording path can tell it apart from
an outage. A status outside 300 to 599 also still fails, because a failure
envelope accepts only those statuses.

**Never read the failed response's body.** The failure text is built from
the requested URL, the status or Go transport error, and the existing
credential explanation, capped at 8 KiB and forced to valid UTF-8. An error
body can't carry anything the token unlocked into storage. A transport error
can still quote bytes the server sent (a malformed status line, for one), so
the prompt names `source.failure` as evidence beside `body`, under the same
ignore-embedded-instructions rule, rather than as daemon authority.

**Keep `Fetch` failing for the task namer.** A transient miss there should be
retried by reconcile, not frozen into an immutable failure artifact that
names the task from nothing.

**Keep `failure` `omitempty` and last in the envelope.** Stored envelopes
are checked by re-marshaling to the exact stored bytes, so a new non-empty
field would break every existing research artifact's decode. Decode and
`recover` accept only a success (2xx, no failure) or a failure (failure
text, no body or content type, status 0 or 300 to 599).

**Fit the specifier prompt line inside its 4 KiB budget.** The prompt was
1 byte under `TestPhase1ASpecifierPromptLeavesEnvelopeHeadroom`'s budget,
and its instruction to treat only `body` as evidence would have hidden
`source.failure`. The new clause says what a failure means and to re-request
only a transient one. To fit, the direct-research rule lost its rationale
clause and the research paragraph lost "Policy may reject URLs or
responses.", since a rejection fails the run before the specifier sees it.
Raising the budget was rejected: it is a check, not this unit's subject.

## Refute-First Findings

A fresh-context reviewer tried to break each claim below, with scratch
tests outside the tree.

- **Confirmed, fixed: server text reaches `source.failure`.** An allowlisted
  host answering with a crafted status line puts that text in the failure via
  Go's `malformed HTTP response` error, and the first prompt draft framed the
  field as the daemon's explanation. The prompt now lists it as evidence.
  Recording a fixed cause class instead of the Go error was rejected: the
  error text is what tells the specifier whether a retry can help.
- **Disproved: the token reaches another spelling.** Twenty-two spellings
  (listing, trailing slash, dot and encoded segments, `+1`, `;x`, case, an
  overflowing number, a trailing-dot host, userinfo) stay unauthenticated;
  every authenticated request goes to the scope's repository ID.
- **Disproved: the token or a mint secret enters a failure.** `url.Error`
  renders the URL, never headers; mint errors render IDs, paths, statuses,
  and scope names only.
- **Disproved: a refusal is recorded.** A non-public dial on a redirect hop,
  an offsite redirect, and redirects to an IP or to `http` all satisfy
  `errors.Is(err, ErrResearchURLRefused)` through `client.Do`, over HTTP/1.1
  and HTTP/2, because the transport doesn't wrap dial errors without a proxy.
- **Disproved: decode or replay confusion, or a task-namer failure artifact.**
  Both decode and `recover` gate on the outcome check; an explicit empty
  `failure` fails the canonical re-marshal; the namer calls `Fetch` under its
  own producer ID.
- **Allowed: `final_url` on a later-hop transport error is the first hop.**
  The failure text names the hop that failed, and nothing reads `final_url`
  from a failure except the specifier.

## Revisit When

- GitHub answers a `pull_requests: read` request with a different grant.
  The mint then fails as `ErrGrantMismatch`, the fetch goes unauthenticated,
  and a private pull request comes back as a recorded 404 instead of a run
  failure. The `failure` text names the mint error; that is the only signal.
- `/repositories/<id>/pulls/...` stops behaving like the owner/name form for
  authenticated reads. Checked unauthenticated on a public repository only.
- Specifiers spend iterations re-requesting failing URLs often enough that
  the prompt line alone isn't a sufficient brake.
