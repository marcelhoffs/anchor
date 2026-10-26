#!/bin/sh
# A binary carries its modules' licenses: collect them, and Anchor's own,
# into licenses/ for the release archives (the Dockerfile does the same).
set -e
rm -rf licenses
GOOS=linux go list -deps \
  -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' ./cmd/anchor | sort -u |
while read -r path dir; do
  mkdir -p "licenses/$path" && cp "$dir"/LICENSE* "$dir"/COPYING* "$dir"/NOTICE* "licenses/$path/" 2>/dev/null || true
done
cp LICENSE licenses/LICENSE
