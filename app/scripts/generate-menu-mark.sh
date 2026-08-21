#!/usr/bin/env bash
# Renders Apps/macOS/FreesideMenuMark.png, the menu-bar template source,
# from the §15 one-color key (Apps/macOS/FreesideKeyMono.svg). The key is
# drawn at 20pt, below the ~24px where its pierced dot survives, so the
# render retires the dot (the last subpath) per the small-size rule:
# slab F kept, nib plain, dot retired. Needs rsvg-convert and magick.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source="$here/../Apps/macOS/FreesideKeyMono.svg"
out="$here/../Apps/macOS/FreesideMenuMark.png"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Drop the dot subpath (the trailing "M240.1 ... Z" arc) from the path.
sed -E 's/ M240\.1 295\.45 A[^Z]*Z//' "$source" > "$work/solid.svg"
rsvg-convert -h 84 "$work/solid.svg" -o "$work/key.png"
magick "$work/key.png" -background none -gravity center -extent 96x96 "$out"
echo "wrote $out"
