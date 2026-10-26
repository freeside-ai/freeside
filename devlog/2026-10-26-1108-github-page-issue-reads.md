# Read a Private Repository's Issue From Its github.com Page URL (#2330)

Work unit: #2330. Scope: `daemon/internal/specify`, `daemon/README.md`, and
`devlog/`. This changes which requests carry the managed repository's
installation token, so the note is mandatory. It extends
`devlog/2026-10-23-1007-authenticated-source-issue-research.md` (#748) and
`devlog/2026-10-25-1553-research-reads-pull-requests-and-records-failures.md`
(#2311).

## Problem

The Wave 8 exit run's fixture pass on 2026-10-26 (#1613) ran the specifier
on `claude-haiku-5-5` against a private fixture repository. The task file
holds the issue's bare page URL, as the walkthrough requires, and the
specifier requested exactly that URL. The fetcher authenticated only the
`api.github.com` origin, so the page went out bare, 404'd, and the specifier
opened an `agent_question`. An earlier pass on Opus requested the API form
and never hit it, so a private repository's specification depended on which
URL form the model chose.

## Decisions

**Rewrite in the fetcher, over a prompt line naming the API form.** The task
file carries the page URL by walkthrough requirement and operators paste page
URLs. A prompt line binds only the models that follow it; a fetcher rewrite
is deterministic for every writer and model. Decided at planning, 2026-10-26.

**Rewrite to the repository-ID API endpoint, over sending the token to
`github.com`.** The token is bound to the repository ID, and a page URL has
no ID form, so an authenticated page request could only be matched by
owner/name, which #748 rejected because a rename can move a name. Matching
the page by owner/name is safe here only because the request that carries
the token goes to `/repositories/<id>/...`.

**Accept exactly `<owner>/<name>/issues/<n>` and `<owner>/<name>/pull/<n>`
with no query, over also mapping page sub-paths.** These are the forms a task
file or operator pastes. Page sub-paths such as `pull/<n>/files` have no
one-to-one API counterpart, and a query on a page means something different
from one on the API. Those, a trailing slash, the API's `pulls` spelling on
`github.com`, and `www.github.com` go as written, without credentials, under
the same plain-segment rules as the API forms.

**Send the page as written when `https://api.github.com` is not allowlisted,
over refusing it or rewriting anyway.** `fetch` validates the final URL
against the allowlist, so rewriting would refuse a public page read that
works today and send egress the policy did not admit. Sending as written
keeps today's behavior; a private issue then 404s, which the README's
allowlist requirement already covers.

**Rewrite even with no token source composed.** A public issue then reads
as API JSON rather than the HTML page. That is smaller, the same shape the
API form returns, and keeps scope decisions independent of credential
availability. The specifier prompt already treats research `body` as
evidence whatever its shape.

## Refute-First Findings

A fresh-context reviewer tried to refute five claims against the diff, with
probe tests through the real `Fetch` and `FetchOrRecordFailure` and a real
`http.Transport` (deleted afterwards).

- **Disproved: the token reaches a host other than `api.github.com`.** The
  rewritten target is built from constants, the scope's ID, and a
  `positiveDecimal`. A 401 or 403 retry and a redirect from a rewritten page
  URL both drop the token on the second request.
- **Disproved: a page spelling routes the token to another repository or
  resource.** Mixed-case `Issues` and `Pull`, `;` parameters, `%37`, `+7`,
  `-7`, an overflowing number, a `.git` suffix, `%00`, fullwidth digits,
  backslash dot segments, and `GITHUB.COM.` go as written without a token.
  `:0443`, `:8443`, userinfo, `github%2ecom`, and fragments are refused by
  the allowlist. Unicode case folding in `strings.EqualFold` (Kelvin sign,
  long s) is unreachable because `plainPathSegment` admits ASCII only.
- **Disproved: the rewrite sends egress to `api.github.com` when the
  allowlist omits it.** The page branch requires the API origin;
  `TestFetcherSendsAGitHubPageAsWrittenWithoutTheAPIOrigin` pins it.
- **Disproved: the token enters an envelope, error, or log.** A recorded
  failure from a page URL stores the page `url`, the API `final_url`, and the
  credential explanation only; the fetcher logs nothing.
- **Disproved: recovery replays a different URL or breaks old artifacts.** A
  page artifact stored before this change (`final_url` the page) recovers
  with the same digest and no request; a different page under the same
  invocation ordinal is rejected as disagreeing.
- **Allowed: a bare trailing `?` is rewritten.** `issues/7?` has an empty
  query, so it is the bare form for every purpose that matters, and the token
  still goes only to the in-scope endpoint.
- **Confirmed, pre-existing and outside this unit: Unicode host lowercasing
  bypasses the allowlist.** `canonicalOrigin` uses `strings.ToLower`, which
  maps `İ` (U+0130) to `i`, so `gİthub.com` passes as `github.com` while the
  transport dials `xn--github-qyd.com`. No token goes there: an in-scope page
  under that spelling is rewritten to the real `api.github.com`, and every
  other request carries no credential. Follow-up: #2340.
