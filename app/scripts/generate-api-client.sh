#!/usr/bin/env bash

set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_dir="$(cd "$app_dir/.." && pwd)"
schema_mirror="$app_dir/Sources/FreesideAPI/openapi.yaml"

cp "$repo_dir/api/openapi.yaml" "$schema_mirror"
swift build --package-path "$app_dir" --target FreesideAPI

if [[ -n "$(git -C "$repo_dir" status --porcelain --untracked-files=all -- app/Sources/FreesideAPI/openapi.yaml)" ]]; then
    echo "Generated API client inputs changed; commit the refreshed schema mirror." >&2
    exit 1
fi
