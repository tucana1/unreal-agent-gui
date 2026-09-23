#!/bin/sh
# Usage: icon.sh icon.png out.icns (macOS: uses sips and iconutil)
set -eu
work=$(mktemp -d)
set=$work/icon.iconset
mkdir "$set"
for size in 16 32 128 256 512; do
	sips -z "$size" "$size" "$1" --out "$set/icon_${size}x${size}.png" >/dev/null
	double=$((size * 2))
	sips -z "$double" "$double" "$1" --out "$set/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$set" -o "$2"
rm -rf "$work"
