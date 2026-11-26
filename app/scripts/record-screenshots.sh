#!/usr/bin/env bash
# Record screenshot digest baselines for named families (or all families),
# replay recording across a branch's commits, or resolve rebase conflicts
# confined to family digest files.
#
# Usage:
#   record-screenshots.sh FAMILY...
#   record-screenshots.sh --all
#   record-screenshots.sh --each-commit <base> (FAMILY... | --all)
#   record-screenshots.sh --resolve
#
# Never force-pushes, stashes, resets, or resolves non-digest conflicts.
set -euo pipefail

PROG=record-screenshots
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd -P)
SCRIPT_ABS=$SCRIPT_DIR/$(basename "$0")
ROOT=$(cd "$SCRIPT_DIR/../.." && pwd -P)
RESOURCES_REL=app/Tests/FreesideCoreTests/Resources
DIGEST_PREFIX=ScreenshotDigests-

usage() {
  cat >&2 <<'EOF'
usage: record-screenshots.sh FAMILY...
       record-screenshots.sh --all
       record-screenshots.sh --each-commit <base> (FAMILY... | --all)
       record-screenshots.sh --resolve
EOF
}

die() {
  echo "$PROG: $*" >&2
  exit 1
}

die_usage() {
  echo "$PROG: $*" >&2
  usage
  exit 2
}

validate_family_name() {
  local name=$1
  [[ $name =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die_usage "invalid family name '$name'"
}

require_macos() {
  [[ $(uname -s) == Darwin ]] || die_usage 'screenshot recording requires macOS'
}

git_path_abs() {
  local rel
  rel=$(git -C "$ROOT" rev-parse --git-path "$1")
  if [[ $rel == /* ]]; then
    printf '%s\n' "$rel"
  else
    printf '%s/%s\n' "$ROOT" "$rel"
  fi
}

rebase_in_progress() {
  [[ -d $(git_path_abs rebase-merge) || -d $(git_path_abs rebase-apply) ]]
}

any_git_op_in_progress() {
  rebase_in_progress \
    || [[ -f $(git_path_abs MERGE_HEAD) ]] \
    || [[ -f $(git_path_abs CHERRY_PICK_HEAD) ]] \
    || [[ -f $(git_path_abs REVERT_HEAD) ]]
}

# Run one swift test pass against surfacesMatchRecordedPixels.
# Args: <record|verify> <0|1 for all> [family...]
run_screenshot_pass() {
  local action=$1 all_flag=$2
  shift 2
  local families=("$@")
  (
    unset FREESIDE_SCREENSHOT_SHARD FREESIDE_RECORD_SCREENSHOTS FREESIDE_SCREENSHOT_FAMILY
    if [[ $action == record ]]; then
      export FREESIDE_RECORD_SCREENSHOTS=1
    fi
    if [[ $all_flag == 0 ]]; then
      local csv
      csv=$(IFS=,; printf '%s' "${families[*]}")
      export FREESIDE_SCREENSHOT_FAMILY=$csv
    fi
    exec swift test --package-path "$ROOT/app" \
      --only-use-versions-from-resolved-file \
      --filter ScreenshotRegressionTests/surfacesMatchRecordedPixels
  )
}

record_and_verify() {
  local all_flag=$1
  shift
  require_macos
  run_screenshot_pass record "$all_flag" "$@"
  run_screenshot_pass verify "$all_flag" "$@"
}

collect_target_paths() {
  local all_flag=$1
  shift
  TARGET_PATHS=()
  if [[ $all_flag == 1 ]]; then
    local f
    for f in "$ROOT/$RESOURCES_REL/${DIGEST_PREFIX}"*.json; do
      [[ -e $f ]] || continue
      TARGET_PATHS+=("${f#"$ROOT"/}")
    done
    [[ ${#TARGET_PATHS[@]} -gt 0 ]] || die "no ${DIGEST_PREFIX}*.json files found under $RESOURCES_REL"
  else
    local fam
    for fam in "$@"; do
      TARGET_PATHS+=("$RESOURCES_REL/${DIGEST_PREFIX}${fam}.json")
    done
  fi
}

do_amend_step() {
  local all_flag=$1
  shift
  git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git repository: $ROOT"
  rebase_in_progress || die 'refusing --amend-step outside a rebase'
  record_and_verify "$all_flag" "$@"
  collect_target_paths "$all_flag" "$@"
  if [[ -n $(git -C "$ROOT" status --porcelain -- "${TARGET_PATHS[@]}") ]]; then
    git -C "$ROOT" add -- "${TARGET_PATHS[@]}"
    git -C "$ROOT" commit --amend --no-edit
  fi
}

do_each_commit() {
  local base=$1 all_flag=$2
  shift 2
  local families=("$@")
  require_macos
  git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git repository: $ROOT"
  ! any_git_op_in_progress || die 'refusing --each-commit while a rebase, merge, cherry-pick, or revert is in progress'
  git -C "$ROOT" rev-parse --verify "${base}^{commit}" >/dev/null 2>&1 || die "unknown base commit '$base'"
  if ! git -C "$ROOT" diff --quiet || ! git -C "$ROOT" diff --cached --quiet; then
    die 'refusing --each-commit with uncommitted tracked changes; commit or preserve them before replaying'
  fi

  local exec_cmd
  exec_cmd="$(printf '%q' bash) $(printf '%q' "$SCRIPT_ABS") --amend-step"
  if [[ $all_flag == 1 ]]; then
    exec_cmd+=" --all"
  else
    local fam
    for fam in "${families[@]}"; do
      exec_cmd+=" $(printf '%q' "$fam")"
    done
  fi
  git -C "$ROOT" rebase -x "$exec_cmd" "$base"
}

do_resolve() {
  git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git repository: $ROOT"
  rebase_in_progress || die 'not at a stopped rebase'

  local unmerged=()
  local path
  while IFS= read -r -d '' path; do
    [[ -n $path ]] && unmerged+=("$path")
  done < <(git -C "$ROOT" diff --name-only --diff-filter=U -z)

  [[ ${#unmerged[@]} -gt 0 ]] || die 'no unmerged files to resolve'

  local digest_paths=()
  local non_digest=()
  local families=()
  local family_re="^${RESOURCES_REL}/${DIGEST_PREFIX}([a-z0-9][a-z0-9_-]*)\\.json$"
  for path in "${unmerged[@]}"; do
    if [[ $path =~ $family_re ]]; then
      digest_paths+=("$path")
      families+=("${BASH_REMATCH[1]}")
    else
      non_digest+=("$path")
    fi
  done

  if [[ ${#non_digest[@]} -gt 0 ]]; then
    {
      echo "$PROG: refusing to resolve: unmerged path(s) outside screenshot digest families:"
      printf '  %s\n' "${non_digest[@]}"
    } >&2
    exit 1
  fi

  local unique_families=()
  local fam
  while IFS= read -r fam; do
    [[ -n $fam ]] && unique_families+=("$fam")
  done < <(printf '%s\n' "${families[@]}" | LC_ALL=C sort -u)

  record_and_verify 0 "${unique_families[@]}"
  git -C "$ROOT" add -- "${digest_paths[@]}"
  echo "$PROG: re-recorded and staged conflicted families: ${unique_families[*]}. Run 'git rebase --continue' to resume."
}

# Parse target list (either --all or one or more FAMILY names) into
# PARSED_ALL (0|1) and PARSED_FAMILIES.
parse_targets() {
  PARSED_ALL=0
  PARSED_FAMILIES=()
  [[ $# -gt 0 ]] || die_usage 'expected --all or at least one family name'
  local arg
  for arg in "$@"; do
    case "$arg" in
      --all)
        PARSED_ALL=1
        ;;
      -*)
        die_usage "unexpected option '$arg'"
        ;;
      *)
        validate_family_name "$arg"
        PARSED_FAMILIES+=("$arg")
        ;;
    esac
  done
  if [[ $PARSED_ALL == 1 && ${#PARSED_FAMILIES[@]} -gt 0 ]]; then
    die_usage 'cannot combine --all with explicit family names'
  fi
  if [[ $PARSED_ALL == 0 && ${#PARSED_FAMILIES[@]} -eq 0 ]]; then
    die_usage 'expected --all or at least one family name'
  fi
}

[[ $# -gt 0 ]] || { usage; exit 2; }

case "$1" in
  -h|--help)
    usage
    exit 0
    ;;
  --resolve)
    [[ $# -eq 1 ]] || die_usage '--resolve takes no additional arguments'
    do_resolve
    ;;
  --each-commit)
    [[ $# -ge 3 ]] || die_usage '--each-commit requires <base> and --all or FAMILY...'
    base=$2
    shift 2
    parse_targets "$@"
    if [[ $PARSED_ALL == 1 ]]; then
      do_each_commit "$base" 1
    else
      do_each_commit "$base" 0 "${PARSED_FAMILIES[@]}"
    fi
    ;;
  --amend-step)
    shift
    parse_targets "$@"
    if [[ $PARSED_ALL == 1 ]]; then
      do_amend_step 1
    else
      do_amend_step 0 "${PARSED_FAMILIES[@]}"
    fi
    ;;
  *)
    parse_targets "$@"
    if [[ $PARSED_ALL == 1 ]]; then
      record_and_verify 1
    else
      record_and_verify 0 "${PARSED_FAMILIES[@]}"
    fi
    ;;
esac
