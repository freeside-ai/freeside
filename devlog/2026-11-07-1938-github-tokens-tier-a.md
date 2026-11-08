# GitHub Tokens Outside Publish Are Tier A

Work unit: #2781. Mandatory note: it changes the project's safety policy
(which files get the Tier A bar). The list lives in AGENTS.md (Assurance
Tiers, Tier A Paths); this note records the rule behind it, the files it
leaves out, and why. It applies the "unlisted file joins the list" rule
of `2026-11-03-1442-assurance-tiers.md`; it changes no tier definition.

## Decision: The Rule

Chose to list every file outside `internal/publish/` that does one of
these, over listing only the files that reveal a token:

- **Reveals or sends a GitHub token.** The bytes of a token leave the
  redaction boundary there, so a mistake leaks a credential.
- **Opens the keystore.** `Keystore.ListApps` returns `AppCredentials`
  with the decrypted App private key and secrets in it, so the key is in
  reach there even though today's callers read only the App's identity.
- **Decides where a token-bearing request may go.** A setting that keeps
  a credentialed request off redirects or credential helpers guards the
  token as directly as the code that attaches it.
- **Chooses which token source a component gets.** The source fixes the
  permission set, so choosing it is granting authority. This is the
  reasoning that already makes `internal/engine/agent_admission.go`
  Tier A: choosing an agent chooses which credential runs.

The mint gate lives in `publish`, so a reveal-only list looks enough. It
isn't: a wiring edit in `cmd/freesided` that hands a consumer
`CachedTokenSource` (publish permissions) instead of a read-only source
grants write authority, and no check in `publish` sees the swap, because
the consumer asks the source for a token and the source mints what it was
built for. Only a consumer that checks the permission set itself refuses
it: projectimage does, `specify` doesn't.

## Decision: The Four Composition Files Are Tier A

`cmd/freesided/onboard.go`, `setup.go`, `preflight.go`, and
`fake_publication.go` open the keystore and build the minter, janitor, or
token source a component gets, so the rule makes them Tier A, like
`claude_driver.go`. This is the agent's default under the rule; the owner
may veto any of them, and the list then follows with the reason recorded
here.

The rejected option was to keep them Tier B because the mint gate is in
`publish`. The paragraph above is why: the gate cannot see which source a
composition file hands out.

**Cost.** `preflight.go` is 2,348 lines and had 22 commits in the 30 days
before this change; `onboard.go` is 873 lines with 9. Most of their edits
touch no credential, yet by-path classification now gives each of them the
Tier A bar. The remedy is to move the credential wiring into small files
so the rest returns to Tier B. That is a code change and a separate unit.
Follow-up: #2840.

## What Is Listed

- `internal/projectimage/source.go`, `identity.go`, `auth.go`, and
  `builder.go`: the clone and identity requests that send a
  `WorkflowAuditPermissions` token, the check that refuses any other
  permission set or repository, and the builder option that carries the
  source.
- `internal/specify/fetcher.go`: sends an `IssueReadPermissions` token on
  in-scope research fetches of the managed repository.
- `cmd/freesided/prelaunch.go`: builds an issue-inspection source
  (`IssueInspectionPermissions`) and sends its token to read one issue.
- `internal/gitrun/runner.go`: `TransportBaseline` sets
  `http.followRedirects=false` and an empty `credential.helper` for both
  the projectimage clone and the publish git transport, which carries
  write-scoped tokens. The clone's redirect refusal lives only here. The
  file is 180 lines with one commit in the 30 days before this change, so
  the Tier A bar costs little.
- `cmd/freesided/onboard.go`, `setup.go`, `preflight.go`,
  `fake_publication.go`: composition, above. `setup.go` also hands the
  one-time registration code to the exchange for the App key.
- `cmd/freesided/claude_driver.go` was already listed for admission
  wiring; its reason now also names its GitHub wiring (keystore, minter,
  janitor, and the publish, follow-up-filing, and issue-read sources).

## Exclusions

Each non-test file outside `publish` that touches a GitHub token type or a
`publish` credential constructor, and is not on the list, with the reason:

- **`cmd/freesided/startup.go`.** Passes what `claude_driver.go` built
  into `specify`, the engine, and signet: `issueReadTokens`, the only
  token source that composition exposes, and the already-built publisher,
  publication transport, follow-up filer, and review replier, each a
  concrete type. It opens no keystore, builds no minter or source, and
  has no other source in reach, so it chooses no permission set.
- **`cmd/freesided/scenario_compose.go` and `internal/exec/fake/forge/`.**
  A fixed fake token for the scenario forge, with no keystore and no real
  GitHub.
- **Type, error, and validator importers.** These import `publish` for
  types, errors, validators, or interfaces only, and build, reveal, or
  choose no credential: in `cmd/freesided/`, `deputy_settings.go`,
  `external_review.go`, `native_review.go`, `onboard_poll.go` (an
  `InstallationAuthoritySource` and a comment naming the janitor),
  `publication_formats.go`, `scheduler.go`, and `supervision.go`; the
  `internal/engine/` importers not already listed (`backup_payloads.go`,
  `hold_classification.go`, `production_workflow.go`,
  `publication_authoring.go`, `publication_metadata.go`,
  `publication_source_issue.go`, `publication_successor.go`,
  `publish_block_facts.go`, `workflow_observation.go`);
  `internal/operations/installation.go` and `onboard.go` (the latter holds
  a `WorkflowAuditor` interface the composition files build); and
  `internal/exec/fake/forge/transport.go`.
- **Already Tier A by another entry.** `internal/ward/claude_review.go`
  matches a token-name search, but its token is a Claude review snapshot,
  and `internal/ward/` is listed. `internal/engine/publication_core.go`
  takes a `publish.Transport` and is listed.

Provider and notification credentials (Claude, Codex, ntfy) are out of
scope here; the unlisted files that reveal them go to a follow-up issue.
Follow-up: #2839.

## The Invariant, Checked

At this change's base the plan's GitHub invariants hold:

- Every token revealed outside `publish` is read-scoped:
  `WorkflowAuditPermissions` in projectimage, `IssueReadPermissions` in
  `specify`, `IssueInspectionPermissions` in prelaunch.
- Write-scoped sources (publish and follow-up filing) are built in
  `cmd/freesided` but revealed only inside `publish`.
- Every token is minted by `publish.Minter` and none enters a workspace.
- Redirect handling differs by recorded choice: projectimage and prelaunch
  refuse redirects; `specify` follows allowed ones and strips
  `Authorization`.

`publish/doc.go` now states this boundary instead of "no other component
holds, mints, or transports" GitHub credentials, which stopped being true
when the read-scoped consumers arrived.

## Refute-First Result

A fresh-context reviewer, given the diff and the issue contract, tried
to find an unlisted, unexcluded file and a false reason:

- **Confirmed, fixed: `publish/doc.go` said `publish` alone holds the App
  key.** `AppCredentials` carries the decrypted key to `onboard.go`,
  `setup.go`, `claude_driver.go`, `preflight.go`, and through
  `selectRepositoryAuthority` to `prelaunch.go`. None reads the key. The
  comment now says `publish` alone uses it, and the keystore bullet of the
  rule names the reach.
- **Confirmed, fixed: `internal/gitrun/runner.go` was neither listed nor
  excluded.** It is now listed, with the redirect rule added to the
  rule above.
- **Confirmed, fixed: the `startup.go` exclusion was narrower than the
  file.** It also passes the built publication components; the reason
  now says so.
- **Disproved:** `gh` or `GH_TOKEN` use (none); `http.extraHeader` or
  `x-access-token` outside `projectimage/source.go` (none); a token
  revealed from a `TokenSource` outside the listed files (none); an
  exported `publish` method handing out a `Secret` (none); a write-scoped
  source feeding anything but a `publish` constructor (none); "sent only
  to GitHub" (each consumer sends to a GitHub origin, and `specify` strips
  `Authorization` on redirect); the excluded type importers building or
  choosing a credential (none); the size and churn figures (accurate).

## Revisit When

- A new `TokenSource` consumer or a reveal of a GitHub token appears
  outside the listed files: it joins the list in that PR.
- The credential wiring moves out of `preflight.go` or `onboard.go`: the
  list names the new files and drops the old ones.
