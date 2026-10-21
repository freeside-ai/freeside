#!/usr/bin/env bash
# supervised-paths.sh — refuse paths under a supervised daemon state root.
#
# Sourced, not executed. Defines refuse_supervised_path, which development and
# exercise scripts call on every state path before they create or use it:
#
#   refuse_supervised_path <label> <path> || exit 2
#
# A supervised root is `Library/Application Support/Freeside` (prod) or
# `Library/Application Support/Freeside Dev` (dev), under both $HOME and the
# account's passwd home, as the daemon checks (#1501). The path and each root
# are resolved physically: the nearest existing ancestor through `cd -P`, with
# symlinks followed and a missing tail appended, so a symlink into a root or a
# not-yet-created path under one is refused. The match folds case, which is
# conservative on a case-sensitive volume, and macOS's /System/Volumes/Data
# alias folds onto /. A path that cannot be resolved (a `..` in its missing
# tail, or a symlink loop) is refused, as is every path when no home is known.
#
# This is an early, readable refusal, not the authority: freesided's own
# environment guard still runs, including the hard-link check for database
# sidecars that this helper does not copy.
#
# Bash 3.2 compatible (macOS /bin/bash): no ${x,,}, mapfile, or associative
# arrays.

# supervised_physical_path <path>: prints the absolute physical path, or fails.
supervised_physical_path() {
  local path=$1 tail='' name target hops=0
  [[ "$path" == /* ]] || path="$PWD/$path"
  while :; do
    if [[ -d "$path" ]]; then
      path=$(cd -P -- "$path" && pwd -P) || return 1
      break
    fi
    if [[ -L "$path" ]]; then
      hops=$((hops + 1))
      ((hops <= 40)) || return 1
      target=$(readlink -- "$path") || return 1
      [[ "$target" == /* ]] || target="$(dirname -- "$path")/$target"
      path=$target
      continue
    fi
    name=$(basename -- "$path")
    case $name in
    ..) return 1 ;;
    .) ;;
    *) tail="/$name$tail" ;;
    esac
    path=$(dirname -- "$path")
  done
  path="${path%/}$tail"
  # macOS firmlinks the data volume back into /, and `pwd -P` keeps whichever
  # spelling it was given, so fold the data-volume alias onto the root.
  case $path in
  /System/Volumes/Data/*) path=${path#/System/Volumes/Data} ;;
  esac
  printf '%s\n' "${path:-/}"
}

# supervised_account_home: prints the passwd home, or fails.
supervised_account_home() {
  local user account_home
  user=$(id -un) || return 1
  # Tilde expansion of ~name reads the passwd entry, not $HOME; the name is
  # validated before eval because it is spliced into the expansion.
  [[ "$user" =~ ^[A-Za-z0-9._-]+$ ]] || return 1
  eval "account_home=~$user"
  [[ "$account_home" == /* ]] || return 1
  printf '%s\n' "$account_home"
}

# refuse_supervised_path <label> <path>: returns 0 when <path> is outside every
# supervised root; otherwise prints why to stderr and returns 1.
refuse_supervised_path() {
  local label=$1 path=$2 resolved folded home homes=() tier dir root folded_root
  [[ -z "${HOME:-}" ]] || homes+=("$HOME")
  if home=$(supervised_account_home); then
    homes+=("$home")
  fi
  if ((${#homes[@]} == 0)); then
    echo "refusing $label \"$path\": neither HOME nor the passwd home names a home to check" >&2
    return 1
  fi
  if ! resolved=$(supervised_physical_path "$path"); then
    echo "refusing $label \"$path\": it cannot be resolved to check it against the supervised state roots" >&2
    return 1
  fi
  folded=$(printf '%s' "$resolved" | tr '[:upper:]' '[:lower:]')
  for home in "${homes[@]}"; do
    for tier in prod dev; do
      dir='Freeside'
      [[ "$tier" == prod ]] || dir='Freeside Dev'
      if ! root=$(supervised_physical_path "$home/Library/Application Support/$dir"); then
        echo "refusing $label \"$path\": the $tier state root under \"$home\" cannot be resolved" >&2
        return 1
      fi
      folded_root=$(printf '%s' "$root" | tr '[:upper:]' '[:lower:]')
      if [[ "$folded" == "$folded_root" || "$folded" == "$folded_root"/* ]]; then
        echo "refusing $label \"$path\": it resolves under the $tier state root \"$root\"" >&2
        return 1
      fi
    done
  done
}
