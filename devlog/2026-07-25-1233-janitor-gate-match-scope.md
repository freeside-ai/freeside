# Gate The Registration That Provides The Token, Not The Keystore

Work unit: #291, which #281 filed as its one accepted-by-decision
consequence. Changes the scope of a runtime capability gate on the
credential path, so a note and a refute-first pass are both mandatory.
Prior notes: `devlog/2026-07-25-1126-janitor-registration-isolation.md`
(#281, whose "the unscoped `Resolve` still denies" bullet this
supersedes), `devlog/2026-07-24-2117-installation-authority-snapshot.md`
(#276, which creates the state that makes this reachable).

## Problem

`InstallationResolver.resolve` required janitor coverage of every
candidate registration before any network call, and the unscoped
`Resolve` the minter uses has every keystore registration as its
candidate set. One faulted registration therefore denied minting for
every owner, which #281's refute pass reproduced directly: with 601
faulted and 501 healthy, `Resolve(ctx, "operator")` returned
`ErrJanitorInactive` even though 501 serves `operator` and had passed.

This is not an exotic state. #276's authority store must refuse a
registration its operator-authored snapshot does not name, and
onboarding writes the keystore record before the operator writes that
entry, so *adding* a registration reliably denies minting for every
existing owner until the snapshot catches up.

## Decisions

- **The gate is scoped by actual match, not by guess.** Every
  registration is still enumerated and matched; coverage is then
  required of every registration that produced a match for the
  requested owner. The gate exists to prove cleanup ran for the
  registration a token is minted through, and a registration that
  matched nothing never provides that token.
- **A pre-network floor keeps the property the old gate really
  protected.** If no candidate registration is covered at all, the
  resolve is refused before anything reaches GitHub. An all-uncovered
  candidate set can never produce a usable binding, so refusing early
  costs nothing, and it keeps "a daemon whose janitor is absent,
  starting, or stopped puts no App key on the wire". It also leaves the
  scoped `ResolveRegistration` path bit-identical, since a
  single-registration candidate set makes the floor exactly the old
  gate; onboarding's `ErrJanitorInactive`-means-not-ready handling is
  untouched.
- **Ambiguity detection is answered by ordering, not by exemption.**
  The objection #281 recorded (`keystore.go:476-480`, #279) is that
  narrowing the set resolution picks among can resolve to the wrong App
  or miss an ambiguity. Matching still runs over the complete candidate
  set, so an uncovered registration that *does* match still denies: an
  ambiguity cannot be laundered into a confident single match. What is
  no longer required is coverage of a registration that contributed
  nothing to the match set.
- **The deliberate trade is one read-only listing.** An uncovered
  registration's App JWT now reaches `GET /app/installations`, provided
  some candidate is covered, because whether it could match is only
  knowable from the forge's answer. The janitor already makes that
  exact request for that exact registration on every pass. No token is
  minted through an uncovered registration, and `mint.go`'s
  `allowsRepository` re-gates the winning binding against live
  coverage, so the credential path is gated twice.
- **The claim is scoped where a reader meets it.** The narrowing fixes
  the janitor gate as a source of unrelated denial and nothing else; a
  registration that cannot be enumerated or validated still denies
  every owner. `NewInstallationResolverWithJanitor`'s doc comment says
  so rather than stating the isolation absolutely, and the remaining
  door is #293, not a silent gap.

## Rejected Alternatives

- **Deciding "could match" from local metadata**, the shape #291's
  objective sketched. Only a *private* registration whose `Owner`
  differs is locally decidable; a public registration's installation
  accounts exist only in the forge's answer. Both registrations in
  #281's recorded repro are public, so this narrowing would not have
  fixed the motivating case at all. It also inverts `AppVisibility`,
  which is metadata rather than an authority decision
  (`keystore.go:1451-1452`), from a field that only ever adds
  restrictions into one that removes a check: a registration recorded
  private but since made public, or one whose owner login was renamed,
  would be skipped silently where today it fails closed and loudly.
- **Moving the gate wholly after the fetch, with no floor.** Simpler,
  and it keeps the same match-scoped guarantee, but a daemon with a
  dead or absent janitor would then sign an App JWT per registration on
  every mint attempt before denying, and both existing zero-request
  tests (`janitor_test.go`, `janitor_grants_test.go`) pin the opposite.
  The floor keeps that guarantee where it is load-bearing at the cost
  of one map lookup per candidate.
- **Keeping the all-or-nothing rule and correcting only the doc
  comment**, which #291 offered as its other branch. Rejected on the
  merits: the availability cost is incurred in ordinary operation
  (#276's onboarding order), and the security property the rule was
  protecting survives the narrowing intact.
- **Suppressing an uncovered registration's enumeration and validation
  errors** so it could not deny an unrelated owner either. This is the
  one narrowing that would trade away the ambiguity guarantee: an
  unreadable registration's accounts are unknown, so it may be the
  second registration installed on the requested owner. Filed as #293
  to be decided with that argument answered, not smuggled in here.

## Refute-First Verification (Required for This Risk Class)

Three independent fresh-context lenses, each prompted to disprove the
change: the capability gate and trust boundary, the credential path end
to end, and test strength plus mutation testing.

**Confirmed and fixed**

1. The post-match gate was pinned only at its last element. A mutant
   that skipped every match but the last survived the whole package,
   because the ambiguity fixture happened to place the uncovered
   registration last and a multi-match denial still looks right (it
   falls through to `ErrAmbiguousInstallation`). The ambiguity test now
   runs both coverage orders and asserts the named registration; the
   mutant dies on the added case.
2. `r.janitor == nil ||` in the post-match loop was unreachable, not
   merely untested: the floor returns unless some registration is
   covered, which requires a non-nil janitor. Proven by a lens planting
   a `panic()` in the branch and seeing the package stay green. Dropped,
   with the reason stated at the point of use.
3. The isolation claim was stated absolutely in the constructor's doc
   comment while holding only for a locally-caused fault. Scoped, and
   the remaining door filed as #293 (finding 4 below).

**Declined, with evidence** (raised by review, after the refute pass)

- **"This contradicts plan §5.5's binding contract, which requires the
  daemon to refuse to operate each registration without its always-on
  janitor"** (P1 on #294, citing `docs/plan.md:1350-1354`). Declined:
  `operate` there cannot mean `make any authenticated request`, because
  the janitor's own passes would violate it. `Run` calls
  `withdrawCoverage()` immediately before every `runCycle`
  (`janitor.go:254`), and `runCycle` then contacts GitHub under every
  registration's App JWT, so for the whole duration of every pass the
  daemon makes App-authenticated calls for registrations that nothing
  covers. That is the only way coverage is ever earned; a reading that
  forbids it forbids the janitor. The rule the paragraph states and
  enforces is a **pre-token** gate ("Before minting any installation
  token ... This pre-token gate ..."), and this change preserves it
  exactly: no token is minted through an uncovered registration, and
  `mint.go`'s `allowsRepository` re-gates the winning binding against
  live coverage. No plan amendment is therefore carried here, which
  also keeps this unit inside its declared scope. Were the owner to
  read `operate` as `contact`, the correction is a plan amendment under
  Document gating, not a code revert: the code would then have to
  return to all-or-nothing, since which registrations could match is
  only knowable from the forge.

- **"Read janitor coverage from one consistent snapshot"** (P2 on
  #294). The interleaving is real and the note's original wording was
  wrong to imply otherwise: the floor reads `ActiveFor` once per
  registration, so a pass beginning mid-count can leave `covered`
  nonzero while the map is already empty. The claim is corrected above.
  The code is not, because the floor cannot be made an invariant by any
  amount of atomicity: the requests go out *after* the count, so a pass
  beginning one instant later has the same effect as one beginning
  mid-count, and closing that would mean holding a coverage lock across
  the network calls. What the interleaving produces is read-only
  listings sent while a pass is in flight, which is the state the
  daemon is in for the whole duration of every pass anyway, since the
  janitor withdraws coverage before each one and then contacts every
  registration itself. The token gate is unaffected: it is re-read
  after matching and re-checked by `allowsRepository` at the mint. The
  floor is now documented as a guard rather than an invariant, which is
  the part of the finding that was load-bearing. Widening
  `JanitorStatus` with a snapshot accessor was rejected for the same
  reason #281 rejected widening it for faults, and would not close the
  window regardless.

**Accepted by decision** (not fixed here)

- **An uncovered registration whose listing also fails still denies
  every owner** (reproduced by all three lenses with 401, 500, and
  `repository_selection:"all"`). It is the likely fault shape, so
  #281's harm survives through this door for a forge-side fault. Not a
  regression (it denied before too, as `ErrJanitorInactive`), and
  closing it means deciding the ambiguity question above: #293.
- **One resolve can now spend up to `installationMaxPages` (100)
  authenticated requests on a registration that cannot yield a token**,
  and inherits its latency, where it previously spent zero. Measured at
  exactly 100 against a forge returning full pages. The pagination
  bound and the absence of a per-request timeout are pre-existing and
  apply equally to covered registrations.
- **The error identity for an uncovered non-matching registration
  changes** from `ErrJanitorInactive` to `ErrNoInstallation` (or that
  registration's own hard failure). More accurate, and inert today:
  the only consumers that discriminate are onboarding's two sites,
  both on the unchanged scoped path.

**Rejected by verification** (probed, no failure constructed; not to be
re-raised)

- No binding escapes the gate: the post-match loop covers every element
  of `matches` and precedes the `len(matches)` switch, and the binding
  can only come from `matches`.
- A typed-nil `*InstallationJanitor` is refused with zero forge
  requests through both entry points.
- A coverage flip mid-resolve cannot mint: `mint.go`'s
  `allowsRepository` re-gates the exact registration, installation, and
  repository against live coverage after resolution returns.
- Duplicate App IDs across two owner directories are gated identically
  to before (coverage is keyed by App ID); pre-existing, not a
  regression.
- The scoped path is unchanged: a one-element candidate set makes the
  floor the old gate, so onboarding's poll loop and its
  selected-registration guarantee hold.
- No token escapes an in-flight pass: coverage withdrawal is whole-map
  atomic, so the post-match gate reads an empty map for the duration of
  a pass and denies. (This bullet first claimed the *floor* was equally
  protected, which review correctly refuted: the floor's reads are one
  per registration and share no snapshot, so a pass beginning mid-count
  leaves `covered` nonzero. Corrected below.)
- No new error text carries untrusted account material: the newly
  reachable errors are `APIError` (status and fixed path),
  `ResolutionFailure` (coordinates only), and transport errors naming
  only the base URL and path.
- No existing test went vacuous; the two zero-request tests still kill
  floor mutants, and every pre-existing janitor stub is uniform, so
  none of them ever exercised gate scope.
- `-race -count=2` over the package is clean; the real-janitor test
  uses a one-hour interval so a second pass cannot withdraw coverage
  underneath the resolution it asserts.

## Revisit When

The resolver gains a per-owner index of installation accounts, from the
janitor's own passes or a cache. That is the only thing that would make
"could this registration match?" decidable before the fetch, which
would let the floor and #293's question both be answered without
contacting an uncovered registration at all.
