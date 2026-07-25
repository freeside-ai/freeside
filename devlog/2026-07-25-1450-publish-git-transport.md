# Publish-Lane Git Transport: Token Crossing and Ref Discipline

Work unit: #277 (daemon-side git transport). Scope:
`daemon/internal/publish`. Credential-leak surface; mandatory note.

## Decisions

**Transport mechanism: hardened system-git subprocess, per-lane
runner.** Reaffirms the hostile-importer decision
(2026-07-16-1515-hostile-importer.md): go-git stays rejected as a heavy
dependency in a trust-boundary package, and `gh` was never a candidate
(it authenticates as a user from ambient state; the lane's auth model
is short-lived, single-repo, scope-verified installation tokens). The
publish lane gets its own runner copy (`gitnet.go`), like `verify/`
did, rather than a shared package: extracting one is a `kind:contract`
change out of this unit's scope, and the runners genuinely diverge
(see stderr below).

**Token crossing: per-invocation `GIT_CONFIG_COUNT`/`KEY_0`/`VALUE_0`
environment config carrying `http.extraHeader`** with the documented
`x-access-token` basic form. The child environment is constructed
fresh per invocation (never from `os.Environ()`), the daemon's own
environment is never mutated, and the runner retains nothing between
invocations; consumers of the entry (`git-remote-https`) die with the
invocation. `Reveal()` sits at exactly one crossing site (`tokenEnv`),
matching `forge.do`. Rejected: a credential helper reading the token
from an inherited pipe descriptor — it plants a shell one-liner inside
a trust-boundary runner, is fragile across git's multiple credential
fills, and ends in the same place (the token in git-remote-https
memory) with more moving parts. Rejected: `GIT_ASKPASS`/helper
scripts, which require an executable or config on disk.

**Ref discipline: direct push to the derived identity branch,
create-only.** The issue's stated preference holds:
`convergePR`'s foreign-resource check is PR-layer, keyed by the
identity marker, indifferent to how the branch arrived. The real
hazard of direct pushing is that a plain non-force push would
fast-forward a foreign ref whose commit is an ancestor of the
candidate — a ref move the REST `createRef` path could never perform.
Closed by the create-only lease: `--force-with-lease=<ref>:` with the
empty expectation refuses any push to an existing ref at the protocol
level (proven in `TestPushLeaseRefusesExistingRefAtProtocolLevel`),
with an `ls-remote` pre-observation for the converged-no-op path and
one bounded re-observation to distinguish a concurrent identical
publication from foreign state. Rejected: a staging ref the publisher
promotes — a second ref lifecycle whose crash residue is exactly the
half-created unreported ref acceptance 6 forbids, for no gain.

**Fetch: "the remote holds it" means reachable from the requested base
ref.** Full single-branch fetch (`refs/heads/<base>` onto a private
`refs/freeside/base`), then `rev-parse --verify` plus `merge-base
--is-ancestor` fail closed (`ErrRemoteMissingBase`). No dependence on
GitHub's SHA-in-want behavior; no `--depth` (shallow state complicates
push connectivity, and 1A-scale repos don't need it). The checkout is
plumbing-shaped: detached HEAD at the base, a gc anchor ref, no
working tree — proven against the real importer in
`TestTransportCarriesHostileHandoffCleanly`.

**Errors: transport stderr never crosses out.** Divergence from the
importer's `GitError`, which retains stderr: transport stderr is
remote-influenced text on an authenticated channel, so
`TransportGitError` carries only argv (fixed daemon material), the
exit code, and a refusal enum classified from fixed patterns; the
streams are dropped at the classification site.

**Hardening beyond the importer template:** `protocol.allow=never`
plus a single `protocol.<scheme>.allow=always` (https in production;
`file` only via in-package test construction — the option surface
cannot select it), `credential.helper=` cleared,
`transfer.fsckObjects=true` for network-received objects,
`push.followTags=false`, no alternates via full env replacement.

## Refute-First Verification

An independent fresh-context reviewer ran the refutation lenses
(credential-leak, ref-discipline, injection, race/convergence,
importer contract, fail-closed gaps) against the uncommitted change,
with empirical checks. Findings and dispositions, all resolved before
commit:

- Confirmed, fixed: repository discovery walks upward, so a missing or
  emptied checkout under any ancestor repository silently pinned that
  repository — whose local config an authenticated invocation honors
  (a `url.*.insteadOf` there would re-aim the credential header).
  Reproduced empirically by the reviewer. `pinRepo` now fails closed
  unless the resolved git dir is the checkout's own `.git`
  (regression: `TestPinRepoRefusesForeignGitDir`).
- Confirmed, fixed: a failed stale-lease re-observation was reported
  as `ErrPublicationConflict`, dressing a transient failure as a
  permanent conflict verdict; it now surfaces as the transport failure
  it is.
- Confirmed, fixed: a transient fetch failure left the init'd `.git`
  behind, wedging every retry of that checkout path on the
  existing-repository guard; a failed materialization now removes the
  repository it created (regression:
  `TestFetchBaseFailureDoesNotWedgeTheDir`).
- Confirmed, fixed (docs): acceptance 6's "bounded retry" ownership
  was implicit; the Transport contract now states the transport never
  retries internally and the recovery drain owns retry.
- Fixed on a speculative finding: git re-sends configured extra
  headers to redirect targets, including cross-host; with a non-GitHub
  `RemoteBase` a redirecting remote could pull the credential header
  off-host. `http.followRedirects=false` closes it; renamed
  repositories now fail loudly (resolver drift, not transport's).
- Mitigated on a speculative finding: refusal classification matched
  remote-influenceable stderr, letting a hostile remote forge the
  stale-lease class that gates the converged re-observation; the class
  now matches only porcelain `!` rejection lines on stdout. Other
  classes stay best-effort diagnostics on already-failed invocations.
- Rejected by verification (reviewer constructed no failure):
  credential leak via argv, error renderings, retained state, or disk
  (env triple + redacting error type + pinned fixtures and byte
  scans); ref discipline (empty-expectation lease verified must-not-
  exist at the protocol level, single fully qualified refspec,
  --atomic, no tag following); injection through repo/ref/SHA
  validation gates; the `^{commit}` peel-and-compare closing the
  tag-object case; scheme=file unreachable through `NewTransport`.
- Accepted by decision: the token is visible in the child process's
  environment (same-UID ps/procfs) for the invocation's lifetime;
  inherent to every env/helper-based git auth mechanism, the process
  is daemon-owned and short-lived, and disk or argv is strictly worse.
- Accepted by decision: `ls-remote`'s returned object name is trusted
  only after 40-hex validation and equality comparison against the
  daemon-derived head; a remote that lies about ref state can already
  lie to any observer, and no other field of git output is consumed.

**Push bindings hold by construction, then by re-gate** (Codex rounds
1–2, four findings accepted, one declined): the reconstruction
trust-boundary convention says the exported `Checkout` is a claim, so
PushHead first makes forgery unrepresentable — only FetchBase can mint
a `Checkout` PushHead accepts (unexported provenance bit; a checkout
does not survive the process, re-run FetchBase after a restart), and
the target repository, pushed head, and branch all derive from one
`IdentityInput`, so a branch belonging to another repository, base, or
candidate cannot be expressed. On top of that it re-gates the
checkout's daemon-authored state before any token is spent: the
stamped repo binding (`freeside.transport.repo`), the exact-base
binding (HEAD still at the claimed base), and the head descending from
that base — closing dir-swap and mix-up holes that construction alone
cannot see. Round 3 tightened the
edges: FetchBase claims its directory atomically (`os.Mkdir`, one
winner), so a failed call removes only what it claimed and concurrent
calls on one path cannot interleave; both operations mint the
installation token only after every local gate has passed, so a
rejected call never causes an audited mint or a live credential
(`TestRejectedCallsMintNoToken` pins the ordering); and the live test
carries an explicit per-run nonce instead of relying on second-
resolution timestamps. Declined (rejected-by-verification): a claimed
nil-pointer panic in `TransportGitError` when git fails pre-start;
`os.ProcessState.ExitCode` is nil-receiver-safe and returns -1,
verified empirically.

Round 4 was the third consecutive round on one theme (PushHead
trusting caller-supplied checkout state), which is the signal to
reframe rather than keep adding checks: `Checkout` became a
**capability** instead of a record. Every field is unexported behind
read-only accessors, so outside this package it can only come from
`FetchBase` and can never be repointed; it carries the repository and
base ref it was fetched from, and the identity must match both. The
earlier config/HEAD/ancestry re-gates stay as the filesystem-level
check the type system cannot make (a directory swapped underneath a
valid capability), but the forge-a-struct class is now closed by
construction, pinned mechanically by `TestCheckoutIsSealed`.

Round 5 closed two input-space gaps: a relative checkout path was
claimed against the daemon's working directory but created under the
private scratch every invocation runs in (now resolved absolute up
front), and the refname gate applied its dot/`.lock` rules only to the
whole name, accepting refs like `release/.candidate` that git rejects
— and accepting them *after* the token mint. The gate now applies the
grammar per slash-separated component, and rather than widen the cited
pattern, the fix is pinned by a differential enumeration over the
input space (case, dots, slashes, suffixes, nesting, control and glob
characters) asserting that every name the gate accepts is one
`git check-ref-format` accepts.

Round 6 closed the last credential-redirect vector and tightened
provenance to the instance. The `-c` hardening outranks local config
for the keys it pins, but a checkout's own config is still read, and
unpinned families can redirect an authenticated invocation:
`url.*.insteadOf` rewrites the very URL the credential header rides
to, and `include.path` pulls in arbitrary config. The fix is an
**allowlist of exact keys** (what `git init` writes plus the
transport's marker) checked before every authenticated invocation in
both operations, so anything the daemon did not write fails closed
before a token is minted — a denylist of known-bad families would
have needed extending forever. Provenance also became per-instance
(`owner *Transport`, not a transferable bit): two transports can point
at different endpoints, and a checkout fetched by one must never be
pushed by the other however alike their repository and base-ref labels
look.

Round 7 tightened that gate from a call-site check to an invocation
one: `runAuthed` re-asserts the allowlist immediately before the
credentialed process starts, so the token mint (a network round trip)
is no longer inside the check-to-use window. The window cannot be
closed outright — git offers no way to run one command with the
repository's own config ignored — and the accepted residual is
narrow: exploiting it requires concurrent write access to a 0700
daemon-private directory, i.e. an adversary already running as the
daemon user, who can read the App private key and mint tokens
outright. Recorded here rather than left implicit, since "the check is
before the use" is otherwise indistinguishable from "the check
cannot be bypassed".

Round 8 closed the last credential-in-argv path: `RemoteBase` was
admitted by a `https://` prefix test, so a base carrying userinfo
would have put a credential on every network git process's argv and
into `TransportGitError.Args`, contradicting the transport's own
guarantee that repository URLs are safe argv and error material. It is
now parsed and required to be a plain https origin (no userinfo, host
present, no query or fragment, not opaque), with the refusal proven
not to echo the credential it refused, and the input space enumerated
once as a test rather than patched at the cited shape.

Rounds 9 and 10 were recurrences of round 8 from successively
incomplete fixes, which is a miss to sweep rather than convergence.
The pattern is worth recording because it repeated three times: each
fix redacted the URL component the last leak was found in (userinfo,
then the opaque body, then query and fragment), and the next round
simply moved the secret one component over. Even the enumeration
written at round 9 reproduced the error at the level above, covering
every rejection *branch* but only the userinfo *position*.

The boundary that actually holds is one level up again: **no part of a
rejected remote base is ever rendered.** Redaction is gone; every
refusal names the defect and drops the value (including the
`url.Parse` error, whose `url.Error` embeds its input). The operator
supplied the value and does not need it echoed to know which one it
is, so the diagnostic loss is nil while the leak class closes by
construction. The test is now the product of both axes — every
credential position crossed with every rejection branch — since the
property is "no rejected base is echoed", not "the known credential
fields are stripped".

Round 11 closed the same forgeability gap round 3 opened but did not
finish. Round 3 stopped the stale-lease class from being matched in
stderr, on the reasoning that the remaining classes were diagnostic
labels no decision consumed; that reasoning was wrong for
missing-ref, which `FetchBase` turned into `ErrRemoteMissingBase`, a
definitive "the base drifted, do not retry" verdict a hostile or
merely unlucky remote could manufacture with one line of prose. The
class is gone from the enum entirely: whether a remote holds a ref has
an authoritative structured answer, so a failed fetch asks `ls-remote`
once and only an actually-absent branch yields
`ErrRemoteMissingBase`; everything else stays a retryable transport
failure. The general rule this leaves: a decision may rest on git's
structured output, never on its prose.

Rounds 12 and 13 fixed a destructive path in the opt-in live test
itself: the deferred cleanup was armed before the push and deleted the
derived branch by name, so a conflicting push (the branch already
occupied by another actor) would have had the test delete a ref it
never created. Cleanup is now armed only after this run created the
branch, and reads the ref before deleting so it removes only one still
at this run's head. Round 13 was the same class recurring from the
round 12 fix: the guard used a non-terminating `t.Error`, so execution
fell through and armed the deletion anyway — on a destructive path an
assertion has to be terminating, or it is a comment. The sibling
`cleanupLivePublication` looks similar but is not the same defect: it
deliberately deletes without knowing the head, because its job is
recovering partial state after a simulated kill, and its branch name
is nonce-unique per run.

**Deferred, not declined** (round 4's second P1): `PushHead` does not
resolve `CandidateAuthorization`, so a caller could push a candidate
`Publisher.Publish` would later refuse, and the refusal would not
remove the ref. The gate belongs to `Publish` and duplicating a
trusted gate in a second place is the #52 failure mode, while this
unit's non-goals exclude changing `Publisher`'s gates; nothing calls
`PushHead` yet (the engine wiring is #236). The transport's contract
now states this explicitly at the method, and composing the two behind
the gate is #288.

## Verification Findings Beyond the Refute Pass

- The per-commit green check (`git rebase --exec` over the stack)
  exposed the ambient-git-state class in the unit's own test helpers:
  rebase exec exports `GIT_DIR`, and fixture helpers that build their
  child env from `os.Environ()` passed it through — a fixture commit
  landed on the rebasing worktree's HEAD and a fixture `git init
  --bare` flipped `core.bare` in the shared config (both repaired).
  The publish helpers now drop every ambient `GIT_*` entry
  (`scrubbedGitEnv`); the sibling helpers in importer, integration,
  and ward share the exposure. Follow-up: #286.
- The live run tripped the known live-fixture trap (see the
  freeside-live-github-app context): `newLiveMinter` hardcoded the
  private same-owner registration shape, which the real App (public,
  owner ≠ installation account) can never satisfy. It now takes
  optional `FREESIDE_PUBLISH_LIVE_APP_OWNER` /
  `FREESIDE_PUBLISH_LIVE_APP_VISIBILITY`, defaulting to the legacy
  shape.

## Revisit When

- #236 wires the transport into the engine/drain: the call order
  (PushHead before Publisher.Publish) and failure surfacing move
  there.
- Managed repositories outgrow full single-branch fetches (introduce
  `--filter`/depth with explicit push-connectivity handling).
- GitHub changes the installation-token git-over-HTTPS form (the
  `x-access-token` basic header is the documented contract today).
