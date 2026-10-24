#!/bin/sh
# Renders the app icons iOS and Android need as PNG, for every accent in
# iconColours (internal/web/appicon.go). Run it after adding an accent or
# changing the glyph, and commit the PNGs; a test fails if any is missing.
#
#   internal/web/static/icons/generate.sh
#
# Needs Docker (rsvg-convert in Alpine). Per accent:
#   icon-<accent>-192/512.png       the favicon: rounded square
#   maskable-<accent>-192/512.png   full bleed, the glyph inside the safe zone
#   apple-touch-<accent>.png        180, square: iOS rounds the corners
set -eu
cd "$(dirname "$0")"

# Keep in step with glyph in appicon.go.
glyph='<g fill="none" stroke="currentColor" stroke-width="9" stroke-linecap="round" stroke-linejoin="round"><path d="M33 32 L16 50 L33 68"/><path d="M67 32 L84 50 L67 68"/><path d="M56 25 L44 75"/></g>'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sed -n 's/^[[:space:]]*"\([a-z]*\)":[[:space:]]*{"\(#[0-9a-f]*\)", "\(#[0-9a-f]*\)"},.*/\1 \2 \3/p' ../../appicon.go |
while read -r accent bg fg; do
	svg() { echo "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 100 100\" color=\"$fg\">$1</svg>"; }
	svg "<rect width=\"100\" height=\"100\" rx=\"22\" fill=\"$bg\"/>$glyph" >"$work/icon-$accent.svg"
	svg "<rect width=\"100\" height=\"100\" fill=\"$bg\"/><g transform=\"translate(15 15) scale(0.7)\">$glyph</g>" >"$work/maskable-$accent.svg"
	svg "<rect width=\"100\" height=\"100\" fill=\"$bg\"/><g transform=\"translate(10 10) scale(0.8)\">$glyph</g>" >"$work/apple-touch-$accent.svg"
done

docker run --rm -v "$work:/in:ro" -v "$PWD:/out" alpine:3.22 sh -c '
	apk add -q --no-cache rsvg-convert
	for f in /in/*.svg; do
		n=$(basename "$f" .svg)
		case $n in
		apple-touch-*) rsvg-convert -w 180 -h 180 -o "/out/$n.png" "$f" ;;
		*)
			a=${n#*-}; kind=${n%%-*}
			for s in 192 512; do rsvg-convert -w $s -h $s -o "/out/$kind-$a-$s.png" "$f"; done ;;
		esac
	done
	chown '"$(id -u):$(id -g)"' /out/*.png'
