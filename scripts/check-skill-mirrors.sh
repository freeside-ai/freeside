#!/usr/bin/env bash
# check-skill-mirrors.sh — fail when .agents/skills drifts from .claude/skills.
#
# Usage: check-skill-mirrors.sh [<repo-root>]
#
# The repository's skills are edited in .claude/skills/ (Claude Code) and
# mirrored as plain copies in .agents/skills/ (Codex). The mirror used to be
# relative symlinks, but Freeside's own workspace seed refuses a tracked
# symlink (#339), so a symlink anywhere in the repository keeps Freeside
# from working on it. Copies drift silently, so this check holds them equal
# (devlog/2026-11-16-1610-skill-mirror-copies.md):
#
#   - every directory under .claude/skills/ has a directory of the same
#     name under .agents/skills/ with identical contents (diff -r, which
#     ignores file modes);
#   - every entry under .agents/skills/ has a source under .claude/skills/;
#   - neither tree, nor .claude/ or .agents/ themselves, is or contains a
#     symlink. Symlinks elsewhere in the repository are out of its scope.
#
# It reads the filesystem, not git, so an untracked file can fail or satisfy
# it locally; CI's clean checkout is authoritative. It performs no network
# I/O and does not mutate the repository.
#
# Exit codes:
#   0  every skill is mirrored exactly
#   1  one or more problems, each listed
#   2  usage/environment error: <repo-root> is not a directory, or
#      .claude/skills/ is missing or holds no skill (so a moved tree can't
#      pass vacuously)
set -euo pipefail

PROG=check-skill-mirrors

usage() {
  echo "usage: $PROG [<repo-root>]" >&2
}

if [ "$#" -gt 1 ]; then
  usage
  exit 2
fi
root=${1:-.}
if [ ! -d "$root" ]; then
  echo "$PROG: $root is not a directory" >&2
  exit 2
fi
source_dir=$root/.claude/skills
mirror_dir=$root/.agents/skills
if [ ! -d "$source_dir" ]; then
  echo "$PROG: $source_dir not found" >&2
  exit 2
fi

problems=()
skills=0

# A symlinked parent would make both trees one, so every check below would
# compare a folder with itself and pass.
for parent in .claude .agents; do
  if [ -L "$root/$parent" ]; then
    echo "$PROG: $parent is a symlink; Freeside's workspace seed refuses tracked symlinks (#339)" >&2
    exit 1
  fi
done

# A symlink is refused wherever it sits, including as a whole mirror entry,
# so look before anything below follows one.
for tree in "$source_dir" "$mirror_dir"; do
  [ -e "$tree" ] || [ -L "$tree" ] || continue
  while IFS= read -r link; do
    problems+=("${link#"$root"/} is a symlink; Freeside's workspace seed refuses tracked symlinks (#339)")
  done < <(find "$tree" -type l | LC_ALL=C sort)
done

for source in "$source_dir"/*/; do
  [ -d "$source" ] || continue
  source=${source%/}
  [ -L "$source" ] && continue
  name=${source##*/}
  skills=$((skills + 1))
  mirror=$mirror_dir/$name
  if [ -L "$mirror" ]; then
    continue
  fi
  if [ ! -d "$mirror" ]; then
    problems+=(".agents/skills/$name is missing; copy .claude/skills/$name there")
    continue
  fi
  # Diff from the root so the output names repository paths as they are.
  if ! drift=$(cd "$root" && diff -rq ".claude/skills/$name" ".agents/skills/$name" 2>&1); then
    problems+=(".agents/skills/$name differs from .claude/skills/$name:")
    while IFS= read -r line; do
      problems+=("  $line")
    done <<<"$drift"
  fi
done

if [ "$skills" -eq 0 ]; then
  echo "$PROG: $source_dir holds no skill directory" >&2
  exit 2
fi

if [ -d "$mirror_dir" ]; then
  for mirror in "$mirror_dir"/*; do
    [ -e "$mirror" ] || [ -L "$mirror" ] || continue
    name=${mirror##*/}
    if [ ! -d "$source_dir/$name" ] || [ -L "$source_dir/$name" ]; then
      [ -L "$mirror" ] && continue
      problems+=(".agents/skills/$name has no source; edit skills in .claude/skills/$name and copy them here")
    fi
  done
fi

if [ "${#problems[@]}" -gt 0 ]; then
  echo "$PROG: .agents/skills/ does not mirror .claude/skills/:" >&2
  printf '  %s\n' "${problems[@]}" >&2
  exit 1
fi
echo "$PROG: $skills skill(s) mirrored exactly"
