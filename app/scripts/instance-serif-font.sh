#!/usr/bin/env bash
# Regenerates Sources/FreesideCore/Fonts/FreesideSerif-{Medium,Regular}.ttf:
# Source Serif 4 (Adobe release 4.005R) instanced at wght=500 and wght=400,
# both at opsz=20 so the two weights are one design, and renamed "Freeside
# Serif", because the OFL reserves "Source" for unmodified files.
# Needs gh (release download), uv (fonttools), and network access.
set -euo pipefail

release="4.005R"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fonts="$here/../Sources/FreesideCore/Fonts"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

gh release download --repo adobe-fonts/source-serif "$release" \
    -p 'source-serif-*_Desktop.zip' -D "$work"
unzip -qo "$work"/source-serif-*_Desktop.zip -d "$work/src"
variable="$(find "$work/src" -name 'SourceSerif4Variable-Roman.ttf' | head -n 1)"

cat > "$work/rename.py" <<'PY'
import sys
from fontTools.ttLib import TTFont

font = TTFont(sys.argv[1])
style = sys.argv[3]
for record in font["name"].names:
    if record.nameID in (1, 16):
        record.string = "Freeside Serif"
    elif record.nameID in (2, 17):
        record.string = style
    elif record.nameID == 3:
        record.string = f"4.005;ADBO;FreesideSerif-{style}"
    elif record.nameID == 4:
        record.string = f"Freeside Serif {style}"
    elif record.nameID == 6:
        record.string = f"FreesideSerif-{style}"
font.save(sys.argv[2])
PY

instance() { # <wght> <style>
    local weight=$1 style=$2
    local out="$fonts/FreesideSerif-$style.ttf"
    uvx --from fonttools fonttools varLib.instancer --update-name-table \
        "$variable" "wght=$weight" opsz=20 -o "$work/instance-$style.ttf"
    uv run --with fonttools python3 "$work/rename.py" \
        "$work/instance-$style.ttf" "$out" "$style"
    echo "wrote $out"
}

instance 500 Medium
instance 400 Regular
