# Gated Publication Push

Work unit: #288. Scope: `daemon/internal/publish`, `daemon/internal/engine`,
`daemon/internal/integration`, `devlog/`. Trust-boundary work; mandatory note.

## Decisions

**Close the ungated-push hazard with a second capability, not by moving the
transport inside `Publisher`.** #277 left `Transport.PushHead` exported and
reachable by anyone holding a `Checkout`: the transport proves what it can see
locally, and only a doc comment said the candidate must have passed
`Publisher`'s authorization, artifact, and drift gates first. A ref created off
that path is not undone by the later refusal. The chosen shape mirrors #277's
own precedent: `GatedHead` is sealed exactly the way `Checkout` is (unexported
fields, read-only accessors, so outside the package the zero value is the only
constructible one), minted at one site in `Publisher.publish` after
`preparePublication` commits the intent, and required by `PushHead`, which
refuses the zero value with `ErrUngatedPublication` before observing the
checkout or minting a token.

The issue offered the alternative of unexporting `PushHead` and giving
`Publisher` the transport. Rejected: the engine owns the checkout lifecycle
(it chooses the directory, fetches the base, and holds the checkout across the
task's reconciliation and its recovery closure), so that shape inverts the
ownership #298 had just landed, and it would restructure both the workflow and
the drain immediately before #237 builds real audited publication on them. The
capability gets the same guarantee without touching who owns what.

**The capability carries the derived `Identity`, not the `IdentityInput` it
came from.** `IdentityInput.ArtifactDigests` is a slice, so an accessor handing
it back would let a holder mutate the digest set after the gate ran and derive
a different branch. Every field on `GatedHead` is a string or an `Identity`
(itself one unexported digest), so a copy is necessarily identical and there is
nothing behind a pointer to repoint.

**Two capabilities stay two capabilities.** `Checkout` says "this transport
fetched this base"; `GatedHead` says "the authority this transport claimed
cleared this head". Neither is folded into the other, and the engine's
`GitPublicationTransport` keeps its own owner-provenance check on top: it can
forward the gate proof but can neither mint nor weaken it.

**The transport holds one publication authority, claimed once and never
replaceable.** `Transport.AuthorizePublisher` claims it, the mint stamps the
issuing publisher and the transport that publisher gates for, and `PushHead`
compares both against the authority the *transport* holds — so no field of the
capability vouches for itself. A transport whose authority was never claimed
publishes nothing, so a wiring that forgets fails closed.

This reverses a decision recorded earlier in this note's own life, and the
assumption that changed is the point. The original reasoning was that the
transport "has no way to know which `Publisher` is authoritative", so an owner
check would be unenforceable decoration and candidate binding carried all the
meaning. Codex's P1 on PR #322 refuted it: `Publisher`'s collaborators are all
exported interfaces and its approved-recipe set is a caller argument, so anyone
holding a real `Transport` can assemble a second publisher whose gates pass by
construction, take the capability from its callback, push it through the real
transport, and return an error so the sham publisher never reaches the forge.
Sealing the type proves only "some `Publisher` gated this head", which is not
the claim the transport needs.

**One-shot, because a settable authority is a nominable one.** The first fix
was a caller-settable `Publisher.BindTransport`, and Codex's second P1 refuted
that in turn, correctly: the rogue publisher just calls
`BindTransport(realTransport)` on itself and its capability then carries the
very pointer `PushHead` accepts. That was not a new hazard but the same one,
still open because the fix had been drawn too narrow — with a capability the
question is never "is the binding present" but "who can set it". Hence the
final shape: the authority lives on the transport, the credential-bearing side,
and the claim is one-shot, so holding the transport confers no power to
nominate its authority and none to displace the daemon's. A rogue claiming
first does not win either; it makes the daemon's own wiring fail loudly at
startup rather than be silently impersonated. Both rounds were reproduced by
deleting the respective check: the foreign publisher's push creates the ref.

## Accepted Limits

- A `GatedHead` proves the gates passed for this head, not that they still
  pass at push time, and it does not expire. The window is the callback's own
  (Publisher hands the capability straight to it and it is used inside that
  call), and the create-only lease plus the gates re-running on every
  publication attempt keep the outcome convergent. Single-use would require
  `Transport` to share mint state with `Publisher`, which buys nothing against
  a caller that already holds the capability legitimately.
- The recovery drain re-runs the gates on the recovered candidate, so
  `RecoveryCandidate.PublishHead` receives a freshly minted capability; a crash
  never carries an old gate decision forward. This is a property of the drain
  calling `PublishAfterGate`, not something the capability itself enforces.
- The authority is claimed by a call at wiring rather than by construction, so
  a wiring can forget it. That fails closed (the transport refuses every
  capability) but at first publication rather than at construction, which is
  the cost of not folding the transport into `NewPublisher`'s already-long
  parameter list. Revisit when a second production wiring of a
  transport-backed publisher appears: one forgetful wiring caught in review is
  cheap, two is a pattern.
- **The boundary this does not cross: credentials.** A caller holding the
  daemon's `TokenSource` can construct its own `Transport`, claim its
  authority, and push, or skip the package altogether and run git directly.
  Nothing in a capability design prevents that, and it is not the hazard #288
  names: this closes the path where the *daemon's own* transport, holding the
  live installation token, carries out a publication its gates never cleared.
  Whoever holds the credential source is already inside the trust boundary.

## Refute-First Verification

Two passes: an independent fresh-context reviewer before the PR opened, running
the refutation lenses (forgery, lost validation, mint ordering,
replay/aliasing, engine seam, test teeth, credential exposure) with empirical
checks, and Codex over two rounds on the PR.

The finding both the design and the whole first pass missed is the authority
decision above, and its shape is worth carrying forward. Every lens asked
whether a caller could *forge* a capability; none asked who could
*legitimately obtain* one. For a capability those are different questions, and
the second is the one that bit — twice, because the first fix answered "is it
bound?" rather than "who can bind it?". The general form: a capability is only
as strong as the least-privileged party who can cause one to exist, so the
lens that matters is "enumerate everyone who can make the system mint one",
not "can this struct be fabricated".

**Confirmed, fixed: the sealed-type test could not see an added exported
mint.** `TestGatedHeadIsSealed` asserts only that no field is exported and that
the accessors read what they should; the reviewer added an exported
`NewGatedHead` to production code and the whole suite stayed green. Sealing the
fields stops construction, but an exported function returning the capability
hands it out just as effectively, and reflection over the type cannot see that.
`TestNoExportedGatedHeadMint` now parses the package's own production sources
and fails on any exported function or method yielding a `GatedHead`; it catches
the reviewer's mutation. This is a test rather than a review habit because the
property has to hold for every future change to the package.

**Confirmed, fixed: `GatedHead{}.Identity().BranchName()` panicked**
(slice bounds on the zero digest), reproduced from outside the package. The
panic surface pre-existed — `Identity` is exported with an unexported field, and
`DeriveIdentity` returns the zero value alongside its error — but this change
adds a second route to it and blesses `Identity()` as part of the capability's
surface. `BranchName` now returns "" for a digest `DeriveIdentity` never
produced, which every branch gate refuses. A sweep for the same
slice-without-guard class in the package found no other instance.

**Confirmed, fixed: `gateHead` accepted an identity and an input it never
checked agree**, making "branch derived from candidate A, repository and head
from candidate B" expressible in-package. No live call site did that, but the
mint now derives the identity itself, so the mismatch is unrepresentable.

**Rejected by verification** (the reviewer constructed no failure): fabricating
a non-zero capability without `unsafe` (struct literal is a compile error,
reflection panics on the unexported field, `json` leaves the zero value, `gob`
refuses a type with no exported fields); `export_test.go` leaking into a
non-test build; validation lost by dropping `DeriveIdentity` from `PushHead`
(all seven of its invariants still run at the mint, and the transport's own
re-gates are unchanged); a gate running after the callback that the pushed ref
would violate (everything after it is convergence, on the same branch at the
same SHA); replay against another candidate, repository, or base ref; the
recovery drain carrying a stale capability across a crash; the engine closures
crossing checkouts between tasks; and any regression in token exposure (the
seal check is the first statement in `PushHead`, ahead of the scratch dir, the
runner, and the mint).

**Accepted by decision:** `unsafe` can set every unexported field of any Go
value, so a same-process caller willing to use it can forge the capability.
That is true of every sealed-struct capability in Go, including #277's
`Checkout`, and is outside this threat model.

## Other Verification Findings

The composed proof needed fixtures from both test packages: `Publisher`'s live
in `publish_test`, the file-scheme remote fixture in `publish`. An
`export_test.go` seam joins them; it compiles into the test binary only, so the
production property that the file scheme is unreachable through `NewTransport`
is untouched. The refusal cases do not themselves depend on the seal (Publisher
never invokes the callback when a gate refuses) — they prove the ordering and
the absence of remote effect, which is the property acceptance 1 names, and the
boundary test proves the seal. Each was mutation-checked: removing the seal
fails the boundary test, moving the callback before `preparePublication` fails
the unauthorized case, and dropping the eligibility call fails the
unapproved-recipe case.

Revisit when a caller needs to hold a `GatedHead` across a boundary the
Publisher's callback does not bound — a queued push, a retry loop that outlives
the publish call, or a second process — since the no-expiry limit above is
scoped to the callback-local use it has today.
