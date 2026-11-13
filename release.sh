#!/bin/sh
# Builds, tests and publishes a GitHub release that Homebrew Channel picks up.
#   1. bump "version" in app/appinfo.json
#   2. ./release.sh            (needs gh, Go, Node and Docker for the e2e test)
# The repository JSON points at releases/latest, so no other file changes.
set -eu
cd "$(dirname "$0")"
VERSION="$(python3 -c 'import json;print(json.load(open("app/appinfo.json"))["version"])')"
TAG="v$VERSION"
if gh release view "$TAG" >/dev/null 2>&1; then echo "release $TAG already exists; bump the version first" >&2; exit 1; fi
./build.sh
[ "${SKIP_E2E:-}" = 1 ] || ./test/integration.sh app/bin/norrsken
IPK="dist/com.zucchiniii.norrsken_${VERSION}_all.ipk"
gh release create "$TAG" "$IPK" dist/com.zucchiniii.norrsken.manifest.json \
  --title "Norrsken $VERSION" --notes "${NOTES:-See the README for changes and install steps.}"
