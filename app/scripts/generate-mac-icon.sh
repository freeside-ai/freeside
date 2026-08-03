#!/usr/bin/env bash
# Rebuild the dark Icon Composer source from the accepted light master.
set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_image="$app_dir/Apps/macOS/AppIcon.icon/Assets/Freeside-light.png"
dark_image="$app_dir/Apps/macOS/AppIcon.icon/Assets/Freeside-dark.png"
scratch_dir="$(mktemp -d)"
trap 'rm -rf "$scratch_dir"' EXIT

command -v magick >/dev/null 2>&1 || {
    echo "generate-mac-icon: ImageMagick's magick command is required" >&2
    exit 1
}

dimensions="$(magick identify -format '%wx%h' "$source_image")"
[[ "$dimensions" == "1024x1024" ]] || {
    echo "generate-mac-icon: expected a 1024x1024 light master, got $dimensions" >&2
    exit 1
}

magick "$source_image" \
    -colorspace gray -level 50%,85% \
    -fill black \
    -draw 'rectangle 0,0 264,1023 rectangle 684,0 1023,1023 rectangle 0,0 1023,100 rectangle 0,924 1023,1023' \
    "$scratch_dir/dark-mask.png"

magick -size 1024x1024 xc:'#16120E' \
    \( -size 1024x1024 xc:'#C2912E' "$scratch_dir/dark-mask.png" \
    -alpha off -compose CopyOpacity -composite \) \
    -compose Over -composite -depth 8 -alpha set \
    -define png:color-type=6 -define png:exclude-chunks=date,time \
    "PNG32:$dark_image"

echo "Generated $dark_image"
