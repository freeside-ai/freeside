#!/usr/bin/env bash
# Renders the dev Mac icon's two Icon Composer layers,
# Apps/macOS/AppIconDev.icon/Assets/FreesideDev-{light,dark}.png, from the
# one-color key (Apps/macOS/FreesideKeyMono.svg). Icon D (#1581): a
# safety-yellow plate inside a 45° black and yellow hazard-stripe frame, a
# thin black line between frame and plate so the yellows don't merge, and the
# black key at about 64% of the icon's height. The stripes use each
# appearance's plate yellow. The system mask rounds the outer corners, so the
# line and plate follow a matching inner radius. At 32 px and below the
# frame blurs into a darker edge, which is intended. Needs rsvg-convert and
# magick; output is byte-stable across runs.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source="$here/../Apps/macOS/FreesideKeyMono.svg"
assets="$here/../Apps/macOS/AppIconDev.icon/Assets"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for tool in rsvg-convert magick; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "generate-dev-icon: $tool is required" >&2
        exit 1
    }
done

ink='#17140F'
size=1024
frame=87     # about 8.5% of the icon
line=10      # the black line inside the frame
stripe=100   # stripe period along each axis; the bands run at 45°
outer_radius=229  # about the system mask's corner radius at 1024 px
key_height=655    # about 64% of the icon

sed "s/currentColor/$ink/" "$source" >"$work/key.svg"
rsvg-convert -h "$key_height" "$work/key.svg" -o "$work/key.png"

render() { # <plate yellow> <output>
    local yellow=$1 out=$2
    local line_inset=$frame plate_inset=$((frame + line))
    local line_far=$((size - 1 - line_inset)) plate_far=$((size - 1 - plate_inset))
    local line_radius=$((outer_radius - line_inset))
    local plate_radius=$((outer_radius - plate_inset))
    magick -size "${size}x${size}" xc: \
        -fx "mod(i+j,$stripe) < $stripe/2 ? 0 : 1" \
        +level-colors "$ink,$yellow" \
        -fill "$ink" -draw "roundrectangle $line_inset,$line_inset $line_far,$line_far $line_radius,$line_radius" \
        -fill "$yellow" -draw "roundrectangle $plate_inset,$plate_inset $plate_far,$plate_far $plate_radius,$plate_radius" \
        "$work/key.png" -gravity center -compose Over -composite \
        -depth 8 -alpha set -define png:color-type=6 \
        -define png:exclude-chunks=date,time \
        "PNG32:$out"
}

render '#FFCB05' "$assets/FreesideDev-light.png"
render '#F0BB00' "$assets/FreesideDev-dark.png"
echo "wrote $assets/FreesideDev-light.png and FreesideDev-dark.png"
