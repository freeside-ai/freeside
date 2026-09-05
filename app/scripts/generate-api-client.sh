#!/usr/bin/env bash

set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_dir="$(cd "$app_dir/.." && pwd)"
schema_mirror="$app_dir/Sources/FreesideAPI/openapi.yaml"

cp "$repo_dir/api/openapi.yaml" "$schema_mirror"
swift package \
    --package-path "$app_dir" \
    --only-use-versions-from-resolved-file \
    plugin --allow-writing-to-package-directory \
    generate-code-from-openapi --target FreesideAPI

drift=$(git -C "$repo_dir" status --porcelain --untracked-files=all -- \
    app/Sources/FreesideAPI/openapi.yaml app/Sources/FreesideAPI/GeneratedSources)
if [[ -n "$drift" ]]; then
    echo "Generated API client changed; commit the refreshed schema mirror and generated client." >&2
    exit 1
fi
