#!/usr/bin/env bash
# runtime-diff.sh: say whether an exit run's rehearsal pass carries to a
# newer tip.
#
# Usage: bash scripts/runtime-diff.sh <pass-sha> <run-sha>
# Local verification:
#   bash scripts/runtime-diff.sh 3ad219fec 0ceb010d7
#   bash scripts/test-runtime-diff.sh
#
# A real run may start from a newer tip than its rehearsal pass when this
# command exits 0 and the run record names both SHAs and the verdict line
# (.claude/skills/exit-run/SKILL.md, Rehearse Before The Operator). The pass
# carries only when both hold:
#
#   - The two SHAs build the same freesided. Each SHA's daemon/ tree is
#     extracted with git archive into a temporary directory and built with
#     go build -trimpath -buildvcs=false -ldflags "-X main.version=runtime-diff",
#     and the two binaries' git hash-object digests must match. The fixed
#     version string stands in for the per-SHA one an operator's build
#     stamps. -trimpath drops the extraction paths; -buildvcs=false keeps a
#     TMPDIR inside some repository from stamping that repository's vcs.*
#     settings. The build clears GOFLAGS and sets GOWORK=off, so a
#     caller's build tags or workspace can't hide a change from both
#     builds. This rests on the build being reproducible: the daemon
#     module is pure Go with no replace directive and no build input
#     outside daemon/, and the same toolchain rebuilds it bit for bit. If
#     that stops holding (cgo, a replace, a Go release), the digests differ
#     and the verdict is "changed": a lost pass, never a wrong carry. A
#     changed go.mod toolchain line changes the binary the same way.
#   - The diff touches no image source (images/, scripts/build-*-image.sh,
#     daemon/cmd/freeside-export/), no prompts/ path, and no policy/ path.
#     The settings pin the images, the role prompts are Go constants in the
#     daemon (nothing reads prompts/ yet), and the agent tree lives in its
#     own repository, so this is a conservative flag, not a proof about
#     those artifacts. The comparison covers the host's freesided only: the
#     linker drops code it doesn't reach, and the exporter builds for linux,
#     so an exporter-only change can leave freesided identical. The pinned
#     exporter image is what keeps that from mattering.
#
# The output is the name-status diff, both build digests, and one verdict
# line: "runtime-diff: carries" or "runtime-diff: changed: <reasons>". Both
# builds run even when a path is already flagged, so the record carries
# both facts. The same commit twice carries without building. This command
# reads the caller's repository without changing it: no worktree, no
# checkout, no fetch. Building an old SHA may download its Go modules.
#
# Exit codes:
#   0  the pass carries
#   1  the runtime changed (the differing build or each flagged path is named)
#   2  usage or environment error (no go on PATH, not inside a git worktree)
#   3  git could not read a SHA or a build failed
set -euo pipefail

# Missing objects in a partial clone must fail instead of fetching implicitly.
export GIT_NO_LAZY_FETCH=1

PROG=runtime-diff

fail_env() {
  echo "$PROG: $*" >&2
  exit 2
}

fail_git() {
  echo "$PROG: $*" >&2
  exit 3
}

if [ "$#" -ne 2 ]; then
  echo "usage: $PROG <pass-sha> <run-sha>" >&2
  exit 2
fi
command -v go >/dev/null 2>&1 || fail_env "go is not on PATH"

ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || fail_env "not inside a git worktree"
cd "$ROOT" || fail_env "cannot enter repository root"

resolve_commit() { # <ref> <role>; prints the resolved commit SHA
  local ref=$1 role=$2 sha
  sha=$(git rev-parse --verify --quiet --end-of-options "${ref}^{commit}") \
    || fail_git "cannot resolve $role SHA '$ref' to a commit"
  printf '%s' "$sha"
}

PASS_SHA=$(resolve_commit "$1" pass)
RUN_SHA=$(resolve_commit "$2" run)
echo "pass: $PASS_SHA"
echo "run:  $RUN_SHA"

if [ "$PASS_SHA" = "$RUN_SHA" ]; then
  echo "$PROG: carries (same commit)"
  exit 0
fi

TMP=$(mktemp -d) || fail_env "cannot create a temporary directory"
trap 'rm -rf "$TMP"' EXIT

# Materialize each diff before reading it, so a failed git command never
# reads as an empty change set.
git diff --no-renames --name-status "$PASS_SHA" "$RUN_SHA" >"$TMP/name-status" \
  || fail_git "cannot compare $PASS_SHA with $RUN_SHA"
git diff --no-renames --name-only -z "$PASS_SHA" "$RUN_SHA" >"$TMP/names" \
  || fail_git "cannot compare $PASS_SHA with $RUN_SHA"
echo "changed paths:"
sed 's/^/  /' "$TMP/name-status"

reasons=()
while IFS= read -r -d '' path; do
  case $path in
    images/* | prompts/* | policy/* | daemon/cmd/freeside-export/* | scripts/build-*-image.sh)
      reasons+=("path $path")
      ;;
  esac
done <"$TMP/names"

build() { # <sha> <name>; prints the built binary's digest
  local sha=$1 name=$2
  mkdir "$TMP/$name"
  git archive --format=tar "$sha" -- daemon | tar -x -C "$TMP/$name" \
    || fail_git "cannot extract daemon/ at $sha"
  (cd "$TMP/$name/daemon" \
    && env -u GOFLAGS GOWORK=off go build -trimpath -buildvcs=false -ldflags "-X main.version=runtime-diff" \
      -o "$TMP/freesided-$name" ./cmd/freesided) >&2 \
    || fail_git "go build failed at $sha"
  git hash-object "$TMP/freesided-$name" || fail_git "cannot hash the build of $sha"
}

PASS_DIGEST=$(build "$PASS_SHA" pass)
RUN_DIGEST=$(build "$RUN_SHA" run)
echo "pass build: $PASS_DIGEST"
echo "run build:  $RUN_DIGEST"
if [ "$PASS_DIGEST" != "$RUN_DIGEST" ]; then
  reasons=("freesided differs" ${reasons[@]+"${reasons[@]}"})
fi

if [ "${#reasons[@]}" -eq 0 ]; then
  echo "$PROG: carries"
  exit 0
fi
verdict=''
for reason in "${reasons[@]}"; do
  verdict=${verdict:+$verdict; }$reason
done
echo "$PROG: changed: $verdict"
exit 1
