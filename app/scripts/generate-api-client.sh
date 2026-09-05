#!/usr/bin/env bash

set -euo pipefail

# --against-index switches the drift comparison from a clean-checkout status
# (what CI and `scripts/check.sh app generate` rely on) to regeneration output
# versus the index, so the pre-commit hook accepts a commit that already staged
# the correct output. The default path is byte-for-byte the CI behaviour.
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

cp "$repo_dir/api/openapi.yaml" "$schema_mirror"
swift package \
    --package-path "$app_dir" \
    --only-use-versions-from-resolved-file \
    plugin --allow-writing-to-package-directory \
    generate-code-from-openapi --target FreesideAPI

outputs=(app/Sources/FreesideAPI/openapi.yaml app/Sources/FreesideAPI/GeneratedSources)

if [[ $mode == against-index ]]; then
    # Regeneration output versus the index: `git diff` reports tracked drift
    # and deletions, and the separate `ls-files --others` catches a brand-new
    # generated file that is not yet tracked.
    drift=$(
        git -C "$repo_dir" diff --name-only -- "${outputs[@]}"
        git -C "$repo_dir" ls-files --others --exclude-standard -- \
            app/Sources/FreesideAPI/GeneratedSources
    )
    if [[ -n "$drift" ]]; then
        echo "Regenerated API client differs from the index; stage these paths and commit again:" >&2
        printf '%s\n' "$drift" >&2
        exit 1
    fi
    exit 0
fi

drift=$(git -C "$repo_dir" status --porcelain --untracked-files=all -- \
    "${outputs[@]}")
if [[ -n "$drift" ]]; then
    echo "Generated API client changed; commit the refreshed schema mirror and generated client." >&2
    exit 1
fi
