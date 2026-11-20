#!/usr/bin/env bash
# Export wireframe .dc.html files to PNG with headless Chrome.
# Usage: docs/wireframes/tools/export.sh <dir-with-.dc.html-files>
set -euo pipefail
SRC="${1:?source dir with .dc.html files}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/.."
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
TMP="$(mktemp -d)"
cp "$HERE/dc-static.js" "$TMP/"
mkdir -p "$OUT/src"

for f in "$SRC"/*.dc.html; do
  name="$(basename "$f" .dc.html)"
  cp "$f" "$OUT/src/$name.dc.html"
  sed 's#<script src="./support.js"></script>#<script src="./dc-static.js"></script>#' "$f" > "$TMP/$name.html"
  w=1440
  [ "$name" = "Phone" ] && w=390
  # pass 1: measure full page height
  h="$("$CHROME" --headless=new --disable-gpu --hide-scrollbars --window-size=$w,900 \
        --virtual-time-budget=4000 --dump-dom "file://$TMP/$name.html" 2>/dev/null \
        | sed -n 's/.*data-h="\([0-9]*\)".*/\1/p' | head -1)"
  h="${h:-1000}"
  [ "$name" = "Phone" ] && h=844
  # pass 2: screenshot at that height
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --window-size=$w,$h \
    --virtual-time-budget=4000 --screenshot="$OUT/$name.png" "file://$TMP/$name.html" >/dev/null 2>&1
  echo "$name ${w}x${h}"
done
rm -rf "$TMP"
