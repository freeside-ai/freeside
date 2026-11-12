#!/usr/bin/env bash

set -euo pipefail

# --against-index switches the drift comparison from a clean-checkout status
# (what CI and `scripts/check.sh app generate` rely on) to regeneration output
# versus the index, so the pre-commit hook accepts a commit that already staged
# the correct output. In a git checkout the default path is byte-for-byte the
# CI behaviour.
#
# Without a .git at the repository root, as in a verification room or the
# project image's recipe proof (an exact-commit tree), the default mode instead
# snapshots the generated outputs before regenerating and reports any output
# file regeneration added, removed, or changed. That tree is the commit
# itself, so "unchanged by regeneration" means what git's clean status means
# there. It differs in one way: it can't see an edit to an output that
# regeneration leaves alone. --against-index needs an index, so it refuses to
# run without git.
mode=default
case "${1-}" in
    "") ;;
    --against-index) mode=against-index ;;
    *)
        echo "usage: generate-api-client.sh [--against-index]" >&2
        exit 2
        ;;
esac
if [[ $# -gt 1 ]]; then
    echo "usage: generate-api-client.sh [--against-index]" >&2
    exit 2
fi

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_dir="$(cd "$app_dir/.." && pwd)"
schema_mirror="$app_dir/Sources/FreesideAPI/openapi.yaml"

outputs=(
    app/Sources/FreesideAPI/openapi.yaml
    app/Sources/FreesideAPI/GeneratedSources
    app/Sources/FreesideAPI/TolerantEnums.swift
    app/Sources/FreesideAPI/ContractDigest.swift
    daemon/internal/signet/contract_digest.go
)

# list_output_files <root>: every file under the outputs that exist in <root>,
# relative to it.
list_output_files() {
    local root=$1 path
    for path in "${outputs[@]}"; do
        if [[ -e "$root/$path" ]]; then
            (cd "$root" && find "$path" -type f)
        fi
    done
}

# Decided by the repository's own .git (a directory, or a file in a linked
# worktree) rather than by asking git, so a git error in a checkout stays
# fatal and a tree without .git nested in another repository isn't compared
# against that one.
if [[ -e "$repo_dir/.git" ]]; then
    in_git=1
else
    in_git=0
fi
if [[ $in_git == 0 ]]; then
    if [[ $mode == against-index ]]; then
        echo "generate-api-client.sh: --against-index needs a git checkout" >&2
        exit 2
    fi
    # Taken before the first write, so the comparison below sees every
    # change regeneration makes.
    snapshot=$(mktemp -d)
    trap 'rm -rf "$snapshot"' EXIT
    for path in "${outputs[@]}"; do
        if [[ -e "$repo_dir/$path" ]]; then
            mkdir -p "$snapshot/$(dirname "$path")"
            cp -R "$repo_dir/$path" "$snapshot/$path"
        fi
    done
fi

cp "$repo_dir/api/openapi.yaml" "$schema_mirror"
# Regenerate the compiled-in contract-digest constants from the same spec, so
# a spec change that forgets them is drift here (and in the pre-commit hook).
bash "$repo_dir/scripts/api-contract-digest.sh" --write
swift package \
    --package-path "$app_dir" \
    --only-use-versions-from-resolved-file \
    plugin --allow-writing-to-package-directory \
    generate-code-from-openapi --target FreesideAPI
# Make every generated enum that lists unknown decode an undeclared value as
# unknown (app/Sources/FreesideAPI/UnknownTolerantEnum.swift).
bash "$app_dir/scripts/write-tolerant-enums.sh"

if [[ $mode == against-index ]]; then
    # Regeneration output versus the index: `git diff` reports tracked drift
    # and deletions, and the separate `ls-files --others` catches a brand-new
    # generated file that is not yet tracked.
    drift=$(
        git -C "$repo_dir" diff --name-only -- "${outputs[@]}"
        git -C "$repo_dir" ls-files --others --exclude-standard -- \
            app/Sources/FreesideAPI/GeneratedSources \
            app/Sources/FreesideAPI/TolerantEnums.swift
    )
    if [[ -n "$drift" ]]; then
        echo "Regenerated API client differs from the index; stage these paths and commit again:" >&2
        printf '%s\n' "$drift" >&2
        exit 1
    fi
    exit 0
fi

if [[ $in_git == 1 ]]; then
    drift=$(git -C "$repo_dir" status --porcelain --untracked-files=all -- \
        "${outputs[@]}")
else
    # cmp fails on a file missing from either side, so an added or removed
    # file counts as drift, as an untracked or deleted one does in git.
    drift=$(
        { list_output_files "$snapshot"; list_output_files "$repo_dir"; } |
            sort -u |
            while IFS= read -r path; do
                if ! cmp -s "$snapshot/$path" "$repo_dir/$path"; then
                    printf '%s\n' "$path"
                fi
            done
    )
fi
if [[ -n "$drift" ]]; then
    echo "Generated API client changed; commit the refreshed schema mirror and generated client." >&2
    if [[ $in_git == 0 ]]; then
        printf '%s\n' "$drift" >&2
    fi
    exit 1
fi
