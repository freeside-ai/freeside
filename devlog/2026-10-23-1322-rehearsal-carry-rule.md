# Carry a Rehearsal Pass Across a Tip Advance That Can't Change It (#2221)

Work unit: #2221. Scope: `scripts/runtime-diff.sh` and its suite,
`.claude/skills/exit-run/`, `docs/production-walkthrough.md`, and
`devlog/`. The rule changes what an exit run must prove before the real
run, settled in planning on 2026-10-23, so the note records it.

## Decisions

**Chose a carry rule over rerunning the pass on every advance.** The real
run may start from a newer tip than its rehearsal pass when
`scripts/runtime-diff.sh <pass-sha> <run-sha>` exits 0 and the run record
names both SHAs and the verdict line. The script exits 0 only when both
SHAs build the same `freesided` and the diff touches no image source
(`images/`, `scripts/build-*-image.sh`, `daemon/cmd/freeside-export/`), no
`prompts/` path, and no `policy/` path. Rerunning on every advance costs a
fresh root, an onboarding, and a pipeline run even for the fixture pass
(#2247), and tracker #2228's exit condition asks that an advance that
can't change the runtime not force one. The Wave 8 run on gh-imgup#125
rebuilt three times; the third (#2193, an owner rule, and #2184, tests
only) changed nothing the pass proved.

**Chose a built-binary comparison over a path heuristic.** Ignoring
`_test.go`, `testdata/`, and Markdown under `daemon/` goes stale the day an
embed, a build tag, or a generated file changes what ships. Building each
SHA's `daemon/` tree from `git archive` with `go build -trimpath
-buildvcs=false -ldflags "-X main.version=runtime-diff"` and comparing the
digests is exact and maintains itself. A reproducibility break fails
toward "changed", costing a pass and never carrying a wrong one. The path
list stays as a conservative flag: settings pin the images, the role
prompts are Go constants in the daemon (nothing reads `prompts/` until
#1428), and the agent tree lives in its own repository, so the list proves
nothing about those artifacts.

**Chose a carry check at launch over pinning the build at go-ahead.**
Preflight requires `main` at the tip the run uses, and an in-run fix
rebuilds from the new tip, so a build pinned at go-ahead breaks both.
The in-run fix rule and the environment checkpoint's client rebuild are
unchanged: clients always come from the real run's tip, so `app/` and
`api/` changes never force a pass.

## Evidence

- At `d47948466` on darwin/arm64 with go1.26.10, planning built the same
  SHA from two checkouts with `-trimpath -buildvcs=false` and got identical
  binaries. Default `-buildvcs` stamps `vcs.*` settings in the primary
  checkout and not in a linked worktree, so the flag is required.
- `runtime-diff.sh 3ad219fec 0ceb010d7` builds both to digest
  `c5803c3d8f5b` and carries, so the rule would have saved the third Wave 8
  rebuild. `runtime-diff.sh d47948466 0b139c64f` reports `freesided
  differs` (#2240 changed the daemon).
- The comparison does not cover the exporter. `freesided` imports every
  package `freeside-export` does, but the linker drops code `freesided`
  never reaches and the exporter builds for linux, so an exporter-only
  change can leave the compared binary identical. The real run's pinned
  `images.exporter` is what keeps the pass valid there; the
  `daemon/cmd/freeside-export/` flag catches only the command itself.

## Revisit When

- Prompts or policy move to files the daemon reads at runtime (#1428):
  widen the flagged paths or compare those files.
- `daemon/go.mod` gains a `replace` directive, cgo enters the module, or a
  Go release stops reproducing the build bit for bit.
