# Wave 2 Chain Amendment: Three Capabilities #236 Assumed

Issues: #231 (chain), #271, #276, #277, #236.

Pre-work on #236 (the 1A.1 integration) found that its flow consumes three
capabilities that do not exist, all outside its declared scope
(`daemon/internal/engine`, `daemon/cmd`, the integration harness). The unit's
own Dependencies field says it "consumes, without changing" the lane packages;
that turned out to be true of the packages and false of the capabilities.

## Findings That Changed The Direction

- **No daemon-side git network exists.** Nothing under `daemon/` clones,
  fetches, pushes, or names a remote. `Publisher.Publish` reaches only refs and
  pull requests (`daemon/internal/publish/forge.go`), so it can create a ref only
  for a commit GitHub already holds; the opt-in live test supplies that commit
  through `FREESIDE_PUBLISH_LIVE_HEAD_SHA`. A daemon-re-authored candidate head
  therefore has no route to the managed repository, and the gauntlet's
  daemon-owned base checkout has no route from it. Plan §5.9 already assumes a
  "daemon-side push".
- **The janitor gate has no production implementation.**
  `InstallationResolver.resolve` refuses every registration, public or private,
  unless a `JanitorStatus` reports coverage, and minting resolves through it. The
  only implementation is `InstallationJanitor`, whose
  `InstallationAuthoritySource` and `JanitorRecorder` exist as ports with test
  implementations only: the #263 note deferred their persistence until an
  onboarding consumer existed. No token can be minted by any daemon process
  today.
- **One unreadable keystore record denies all resolution.** The maintainer's
  pre-#245 `freeside-ai` record fails `Keystore.ListApps`, which every janitor
  cycle calls first, so the two findings above cannot even be exercised live
  until #271 lands.

## Decisions

- **Chose decomposition over widening #236,** inserting #271, #276 (janitor
  authority), and #277 (git transport) into the Wave 2 chain ahead of it (owner
  choice, 2026-07-24). Two of the three are high-risk surfaces the repository's
  high-assurance rules put behind their own refute-first pass: the janitor
  suspends and deletes installations with no unsuspend path, and the transport
  carries an installation token into a subprocess. Folding both into the
  integration unit would have put two independent refute-first passes and the
  convergence work under one review, and would have diluted an acceptance stated
  as the §11 1A.1 exit verbatim.
- **Chose an operator-authored state-directory snapshot as the janitor's
  authority over a store schema** (owner choice). The #263 note's reason for
  deferring persistence still holds: no consumer creates or promotes these
  records, so encoding a storage layout now would make the janitor choose the
  onboarding model that #238 owns. The file-backed source also keeps #265's
  epoch and frontier contract free while the movable control plane is
  unscheduled.
- **Chose `daemon/internal/publish` over `daemon/cmd/freesided` as the home for
  both new implementations** (agent judgment, refining the owner's choice). The
  lane that owns the ports owns their first implementations, and it keeps the
  new units' declared paths disjoint from #236's, so the chain's units do not
  contend for `daemon/cmd`. `freesided` composes them, which is #236's work.
- **Chose fiat scheduling for #271** (owner choice) rather than leaving it in the
  unscheduled deferral queue, since every remaining unit in the chain needs a
  live janitor pass and none can run while listing fails closed.

## Rejected Alternatives

- **One large #236 PR carrying transport, janitor wiring, engine workflow, and
  the live run.** Fastest to a real PR on the managed repository, rejected
  because it merges two destructive/credential-surface reviews into an
  integration review and makes the 1A.1 exit's verdict depend on capability work
  the exit list does not name. This is the same reasoning that split #41 out of
  #235 during Wave 2 planning.
- **A `kind:contract` store-backed authority now.** Rejected as premature for
  the reason #263 already recorded, and because a contract unit would serialize
  against every other open contract unit for a shape that #238's onboarding
  transaction should determine.
- **Deferring the live publication and proving 1A.1 only against a scripted
  forge.** Rejected because "daemon GitHub publication to the first managed
  repository" is the exit criterion, and the maintainer prerequisites
  (#232/#233/#234) exist precisely to make it real; an offline-only proof is the
  assumed-rather-than-proven pattern the Wave 1 audit flagged.
- **Treating the missing capabilities as integration bugs under the #231
  Template F rule.** Rejected because that rule routes a *bug traceable to a
  lane's package*; absent capability is unbuilt work, and filing it as a fix
  would understate what the units must verify.

## Revisit When

`freesided onboard` (#238) owns authoring and promoting installation authority,
or the movable control plane's contract (#265) is scheduled: at that point the
state-directory snapshot is the migration's source, and the file-backed source
becomes a compatibility path rather than the authority.
