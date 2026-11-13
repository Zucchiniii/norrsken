#!/bin/sh
# Builds dist/com.zucchiniii.norrsken_<version>_all.ipk
#   1. cross-compiles the daemon for 32-bit ARM (runs on 32- and 64-bit LG TVs)
#   2. packages the web app + binary with ares-package (from @webos-tools/cli)
set -eu
cd "$(dirname "$0")"

VERSION="$(python3 -c 'import json;print(json.load(open("app/appinfo.json"))["version"])')"
BUILD_ID="$(date -u +%Y%m%d%H%M%S)"
echo "== norrsken $VERSION (build $BUILD_ID)"

echo "== daemon (linux/armv7, static)"
(cd daemon && go test ./... >/dev/null && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
  go build -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.buildID=$BUILD_ID" -o ../app/bin/norrsken .)
chmod 755 app/bin/norrsken

echo "== ipk"
mkdir -p dist
rm -f dist/*.ipk
npx --yes -p @webos-tools/cli ares-package --no-minify app -o dist >/dev/null
IPK="$(ls dist/*.ipk)"
echo "$IPK ($(du -h "$IPK" | cut -f1))"

# Homebrew Channel manifest (only needed when publishing a repository)
SHA="$(shasum -a 256 "$IPK" | cut -d' ' -f1)"
cat > dist/com.zucchiniii.norrsken.manifest.json <<EOF
{
  "id": "com.zucchiniii.norrsken",
  "version": "$VERSION",
  "type": "web",
  "title": "Norrsken",
  "appDescription": "NordVPN (NordLynx) for rooted webOS",
  "iconUri": "https://raw.githubusercontent.com/Zucchiniii/norrsken/main/app/largeIcon.png",
  "sourceUrl": "https://github.com/Zucchiniii/norrsken",
  "rootRequired": true,
  "ipkUrl": "$(basename "$IPK")",
  "ipkHash": { "sha256": "$SHA" }
}
EOF
echo "sha256 $SHA"
