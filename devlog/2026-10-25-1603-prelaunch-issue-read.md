# Read The Target Issue In Prelaunch Without A Janitor (#2269)

Work unit: #2269. Scope: `daemon/internal/publish`, `daemon/cmd/freesided`,
`daemon/README.md`, `docs/production-walkthrough.md`,
`.claude/skills/exit-run`, and `devlog/`. Prelaunch now mints a second App
token and sends it as a bearer, and decides a check from fields GitHub
returns, so this is credential-surface and returned-object trust-boundary
work and the note is mandatory. It follows
`2026-10-24-1017-prelaunch-command.md`, which shipped #2222 without this
read.

## Decisions

**Mint from the durable installation-authority binding, over running a
janitor pass or reusing `IssueReadTokenSource`.** `IssueReadTokenSource`
resolves through `Minter.resolveTrusted`, which fails closed unless an
active janitor vouches for the installation, and a janitor pass would write
janitor state and can remove installations, which prelaunch must never do.
`AuthorityInspectionTokenSource` already mints without a janitor from the
exact durable binding, for composition preflight's repository read. The new
`IssueInspectionTokenSource` is its issue-read sibling, and both resolve
through one function, `resolveDurableBinding`, so their fail-closed rules
can't drift. Prelaunch also selects its App through the same helper the
composition uses, `selectRepositoryAuthority`, so the two mints can't pick
different registrations.

**Its own narrow grant, over the research fetcher's (owner decision,
2026-10-25).** `IssueInspectionTokenSource` mints
`IssueInspectionPermissions`, `issues: read` and `metadata: read` only.
#2320 widened `IssueReadPermissions` to `pull_requests: read` so the
research fetcher can read earlier pull requests (#2311); prelaunch reads
only the target issue, so the owner chose to keep it least-privilege
rather than share the wider set. The research grant is unchanged.

**No token cache.** Prelaunch mints once per run, and a cache would be
state to get wrong on a credential path for no saving. Every call
revalidates the registration and binding.

**A pull-request number fails the check, over passing it as open.** The
issues API serves pull requests at the same path. A task naming a PR's
number would pass a state-only check and then fail in the run, which is
the failure this check exists to move earlier. GitHub's App-permission
docs say an App holding issues but not pull-request permission is limited
to issues, so through the narrow grant a PR's number should answer 404,
not a 200 with the `pull_request` marker. By the read, the mint has
already shown the App can read the repository's issues, so the 404
evidence names the two causes left, no such issue or a pull request, and
its fix is "choose an open issue". The marker check stays for a 200 that
carries it. Not
verified with a live read: this session had no test App credential to
use, and prod credentials are off limits.

**Read only issues of the managed repository.** The App's grant covers only
that repository; the specifier reads any other issue without it. Reading
another repository's issue through the App would fail for a reason that
says nothing about the run.

## Refute-First Pass

Each claim below was checked against the change before commit.

- **The token reaches the record, stderr, or an error: disproved.** The
  record carries only `url`, `readable`, and `state`. The token is set only
  on the request's `Authorization` header. Mint errors carry scope names,
  never the token (`TestIssueInspectionTokenSource*` assert no error
  contains the fixture token), and the source's `Format` renders a fixed
  name, so printing it can't reach the keystore.
- **A redirect carries the bearer to another host: disproved.** The read's
  client returns the redirect response instead of following it, and a 3xx
  fails the check as an unexpected status
  (`TestPrelaunchIssueReaderTrustsOnlyTheAnswerItAskedFor/redirect`).
- **An answer about another issue decides the check: disproved.** A 200
  whose `number` differs from the request is an error, and the decoder is
  bounded at 1 MiB, above GitHub's issue-body cap.
- **A wider grant circulates: disproved.** `mintResolved` refuses any
  granted scope map that differs from the request, including the research
  fetcher's `issues`, `pull_requests`, and `metadata` set
  (`TestIssueInspectionTokenSourceRejectsDifferentGrants`).
- **The research grant's widening reaches prelaunch: confirmed, fixed.**
  Rebased over #2320, the source minted the widened `IssueReadPermissions`.
  It now mints its own `IssueInspectionPermissions`, and the production
  reader's fixtures pin `{"issues":"read","metadata":"read"}`.
- **The refactors change behavior: disproved by comparison.**
  `resolveDurableBinding` against the old `AuthorityInspectionTokenSource`
  body, and `selectRepositoryAuthority` against the old selection prefix of
  `inspectRepositoryAuthority`, gave the same result and error text on every
  generated input (temporary comparison tests, not kept).
- **The database is held across the GitHub read: disproved.** The daemon
  lock and store are opened only for the mint and its audit row and closed
  before the GET.
- **A fail-closed constructor like #2222's could pass the tests again:
  disproved.** `TestPrelaunchIssueReaderMintsFromDurableAuthority` builds
  the production reader over a keystore, authority store, and database on
  disk and asserts one issue-read mint and its audit row.
- **A slow composition expires the mint's App JWT: confirmed, fixed.** A
  fresh-context reviewer found the read signed its JWT with the clock taken
  when the checks began, after the whole composition had run; the JWT lives
  minutes. The read now takes the clock when it runs.
- **A duplicated registration can't be built: confirmed wrong, tested.**
  The keystore keys registrations by owner, so one App ID saved under two
  owners lists twice; the resolver refuses it before any mint, and the test
  table now covers it.
- **GitHub's `state` is recorded raw: allowed.** The contract asks for it in
  the record, and only a hostile intermediary on the fixed API host could
  shape it, which is outside the trust boundary the mint's own grant check
  assumes.
- **The resolver's own ambiguity refusal is unreachable through validated
  authority: allowed.** Authority validation already refuses a repository
  or an account bound to more than one installation. The resolver keeps its
  check, carried over unchanged, and the test pins the validation's refusal
  instead.

- **The 404 fix still blamed the App's installation: confirmed, fixed.**
  A fresh-context reviewer found the reworded 404 evidence and its fix
  named an unreadable App, which the successful mint has already ruled
  out; a nonexistent number got "install the prod App". The 404 now names
  no such issue or a pull request, with "choose an open issue". The 410
  fix and a transferred issue's 301 (retry advice) predate this rework and
  are left to a follow-up.

Revisit when: prelaunch needs to read pull requests,
`AuthorityInspectionTokenSource` or this source gains a cache, or
prelaunch starts reading issues outside the managed repository.
