# Align Golden Images Before Real Runs

Issues #325 and #337 resolve one coupled plan contradiction. The agent-base and
project-image constraints had been measured and enforced in image tooling, but
the plan did not own them. The plan also assigned project-image construction to
packaged onboarding after the real runs that already need the image, and still
described the first target as Go after `freeasinbird/gh-imgup` had been selected
on behavior.

## Decisions

**Chose one canonical ward-enforced realized shape for agent bases and project
images.** No contributed or inherited `ENV`, `WORKDIR`, `ENTRYPOINT`, `CMD`,
`USER`, or `VOLUME` may change that shape. Ward inspect-verifies the fields the
runtime exposes; source and build validation must cover the user field that the
current report omits. A project image is checked independently because a
compliant base says nothing about metadata added by its derived layer. The
trusted recipe runs verbatim without network, with the repository dependency
closure and tool configuration baked as files. A negative probe masks the baked
material and must fail by attempting registry or network access, which
distinguishes a load-bearing offline proof from a recipe that never needed the
dependencies.

**Chose a reusable builder before real runs, then packaged that same primitive
in onboarding.** The builder takes an exact repository identity, commit, and
trusted recipe and returns a registry-resolvable digest reference. It is
manually proven against the selected repository before #237, and #238 invokes
it from `freesided onboard` after the run path has survived real use.

Rejected: restoring a checked-in per-project image. That would duplicate a
managed repository's dependency manifest in Freeside, make unrelated dependency
churn a control-plane source change, and repeat for every repository. This
preserves the correction in
[`2026-07-26-2130-project-images-belong-to-onboarding.md`](2026-07-26-2130-project-images-belong-to-onboarding.md)
rather than reviving the frozen predecessor it superseded.

Rejected: letting the manual proof and onboarding use separate image builders.
That would package unproven behavior and allow their recipe and provenance
semantics to diverge.

**Chose registry-backed digest identity with one measured build-time
exception.** Ward receives only `name@sha256:<digest>` references resolvable
through a registry. Apple `container` 1.1.0 cannot use a locally built
`name@digest` as the derived image's base, so the builder may use a tag for that
build-time hop only after verifying its digest and must record the exact base
digest in the result. A local content-store digest or tag never becomes run
authority.

**Chose behavior-based first-repository criteria over a language example.**
`freeasinbird/gh-imgup` remains the selected target because its automation
authority is representable by the current contracts, its trusted recipe can run
offline from a project image, and its ordinary work can cross the gauntlet. The
old Go plus `go test`/`go vet` wording described a preference that the selected
TypeScript repository disproved; retaining it would make the plan disagree with
the completed owner decision in #232.

Revisit when: the ward's runtime changes its observed launch-metadata behavior;
Apple `container` resolves locally built digest references without a registry or
tag hop; or project-image construction needs a second implementation for a
concrete, measured reason rather than packaging convenience.

Follow-up: #334.
