# Project Images Keep Themselves in Service

Plan revision 98. Mandatory note: a material plan and architecture change
(§5.7, §5.12, §5.16, §10, §11, §14), decided by the owner on 2026-10-13 on
#1999 and replanned against `main` at `5b52b49eb` on 2026-10-26. The plan
text is §5.7 **Project-image upkeep**; the follow-on units are #2332 to
#2338, unscheduled, for the spine to place at wave-12 planning.

## Decision

Chose a daemon-owned upkeep job over leaving image drift to attention items a
human resolves by hand. Before this revision the builder and its gate existed
(§5.7 reuse across compatible commits, the policy-gated rebuild, Go images),
but nothing noticed drift no candidate caused: a dependency change merged
outside Freeside, or a daemon upgrade that changes the toolchain digest,
stranded the next run on an attention item, and #1784 named "run the earlier
binary until the task drains" as the only remedy. The owner's direction: a
human reviewing at the merge gate or tapping a card is the normal Freeside
loop, so every judgment case becomes Freeside work, never a manual
`freesided onboard`.

## Choices and Rejected Options

- **Upkeep job over human-resolved items.** One permanent trusted-config job
  with three entry points (default-branch tip advance, daemon start, the
  doctor's schedule as a backstop). An admissible image needs nothing; a delta
  the gate admits is rebuilt and admitted silently. Rejected: an attention
  item per drift, which is the chore the owner asked to remove.
- **The gate also runs on a toolchain-only delta.** Both manifests equal and
  only the recorded toolchain digest differing is what a daemon upgrade
  produces. Such a delta meets every existing gate condition, so this widens
  when the gate runs, not what it admits.
- **Adoption card over an agent task for policy-clause refusals** (owner
  choice). A changed recipe digest or an undeclared registry authority is
  already in the repository; what it needs is a trust decision, not an agent
  producing a change that already exists.
- **The adoption goes through the control-plane gate, not into the trust
  profile.** The owner's choice named a one-tap, digest-bound
  `effect_proposal` into the trust profile. Recipe approval and the registry
  set are trusted project policy (the approved-recipe set in
  `daemon/internal/verify/options.go`, the registry set in project policy),
  which §5.8 lets change only through an operator-authored PR or the §4
  policy-proposal path. So the card's approval opens that proposal and its
  merge is the adoption; the builder then builds anew under the adopted
  policy, as onboarding does. Rejected: rebuilding "under the same gate"
  after adoption, because the gate's unchanged-recipe clause would still
  refuse, and relaxing it is the widening this revision rules out. This
  departs from the owner's choice; the implementer made it.
- **Submission primitive over §5.17 filing for repairs.** A failing positive
  run, masked-cache proof, or deterministic install (#1792's classification)
  needs a repository change, so the daemon submits a maintenance task through
  the §5.12 acceptance primitives with a daemon origin and the refusal record
  as its source. Rejected: §5.17 follow-up issue filing, a human-gated fan-out
  whose issues are propose-only anyway, so it adds two gates and no
  information.
- **Every gate refusal has a class.** The issue named four classes; the
  gate also refuses an inconsistent lockfile and an unpinned or VCS source
  (repairs: the fix is a repository change), a project with no registry set
  (a policy clause), and a missing rebuild configuration or a timed-out
  build (host faults). An unsupported npm input (including a change to
  `package.json`'s `overrides`, `workspaces`, or `packageManager`) is
  usually deliberate and a repair could only revert it, so it ends in the
  blocked outcome. A refusal is policy-only only after every gate clause is
  evaluated, so a recipe change arriving with an unpinned source is a
  repair first. Leaving any refusal unclassified would strand work on the
  old blocked outcome. The implementer chose these classes; the issue
  named only four.
- **A maintenance task runs on the image it fixes.** Its base is the
  refused tip, where no image is admissible, so an ordinary hold would make
  it wait on itself. It is the one exception to composition's admissibility
  check. Its writer starts in the pointed-to image. The refusal record, the
  trusted builder's failed run at that exact tip, stands in for verifying
  the base. The candidate is verified only in an image built and proven at
  the candidate head, under the gate's conditions measured from the
  pointed-to image's build commit, and publication re-derives that image's
  admissibility as for any run. Rejected: verifying the candidate in the
  pointed-to image, because that image no longer matches the tip's
  environment, so a pass there would prove nothing about the tip. The
  implementer added this; the owner's choices did not cover it.
- **Per-project, per-agent-base admitted-image pointer over the
  `-agent-image` flag.** One daemon flag cannot serve two projects, and
  advancing it means an operator restarting the daemon. Project images
  derive from an agent base, and the Codex base gets its own project images
  (§11 wave 9), so one pointer per repository would repeat the flag's
  problem. The pointer picks an image but never vouches for it: image
  records stay write-once, and every `AdmissibleAt` boundary still
  re-derives admissibility from the record and the exact base tree.
- **Holds over blocks while a resolution is pending.** A task whose base needs
  a rebuild, an adoption, or a repair waits under a visible hold that releases
  when an admitted image serves its base. #1784's blocked outcome narrows to
  a refusal with no pending resolution, or a base no admitted image can
  serve.
- **Go module deltas kept refused, with their own gate unit** (#2337).
  The gate's delta rules (`daemon/internal/projectimage/lockfile_delta.go`)
  read npm lockfiles only. Admitting a `go.sum` change at the tip under
  upkeep would admit a delta the gate never checked against the registry set.
  Revision 92 and `devlog/2026-10-21-1215-claude-review-and-bootstrap.md`
  both read #1999 as admitting only a toolchain delta, so the Go extension
  needs its own unit, and the §5.7 Go clause now points at it.
- **Agent-base upkeep designed now, scheduled later** (owner choice;
  #2338). The same loop applies to the agent bases' pinned vendor CLIs,
  but it needs Freeside onboarded against its own repository first.
- **§11 row 12 in 1B.2** (owner choice) over a Phase 2 bullet, because
  unattended operation does not hold over time without upkeep, and over
  folding it into row 11, whose initiative view shares no code with it. The
  1B exit evaluation moves from row 11 to row 12 so it still closes 1B.

## Revisit When

- A recipe shape beyond npm, Go, and Swift lands: the gate's delta rules are
  per-ecosystem, and a fourth shape tests whether the classification above
  still covers its refusals.
- The Go module-source gate (#2337) merges, so the §5.7 Go clause can
  lift and a Go delta becomes an ordinary gate verdict.
- Freeside is onboarded against its own repository, the prerequisite for
  agent-base upkeep (#2338).
- A per-host or per-architecture image record lands (#2161's Linux desktop
  prerequisite) before the pointer contract: the pointer names one registry
  digest, which a manifest list also is, and a per-host record would change
  its shape.
