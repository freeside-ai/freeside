#!/usr/bin/env bash
# Regenerates Sources/FreesideCore/Fonts/FreesideSerif-Medium.ttf: Source
# Serif 4 (Adobe release 4.005R) instanced at wght=500 opsz=20 and renamed
# "Freeside Serif", because the OFL reserves "Source" for unmodified files.
# Needs gh (release download), uv (fonttools), and network access.
set -euo pipefail

release="4.005R"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="$here/../Sources/FreesideCore/Fonts/FreesideSerif-Medium.ttf"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

gh release download --repo adobe-fonts/source-serif "$release" \
    -p 'source-serif-*_Desktop.zip' -D "$work"
unzip -qo "$work"/source-serif-*_Desktop.zip -d "$work/src"
variable="$(find "$work/src" -name 'SourceSerif4Variable-Roman.ttf' | head -n 1)"

uvx --from fonttools fonttools varLib.instancer --update-name-table \
    "$variable" wght=500 opsz=20 -o "$work/instance.ttf"

cat > "$work/rename.py" <<'PY'
import sys
from fontTools.ttLib import TTFont

font = TTFont(sys.argv[1])
for record in font["name"].names:
    if record.nameID in (1, 16):
        record.string = "Freeside Serif"
    elif record.nameID in (2, 17):
        record.string = "Medium"
    elif record.nameID == 3:
        record.string = "4.005;ADBO;FreesideSerif-Medium"
    elif record.nameID == 4:
        record.string = "Freeside Serif Medium"
    elif record.nameID == 6:
        record.string = "FreesideSerif-Medium"
font.save(sys.argv[2])
PY
uv run --with fonttools python3 "$work/rename.py" "$work/instance.ttf" "$out"
echo "wrote $out"
