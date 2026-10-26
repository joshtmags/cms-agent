#!/usr/bin/env bash
# Builds the release for one version into dist/<version>/:
#   manifest.json + cms-arena-agent-<version>.tar.gz (unsigned)
# Used by .github/workflows/release.yml, and locally to try things out.
#
#   scripts/build-release.sh v1.2.3 [--rollback]
set -euo pipefail

tag="${1:?usage: build-release.sh vX.Y.Z [--rollback]}"
version="${tag#v}"
shift
cd "$(dirname "$0")/.."

out="dist/$version"
stage="$out/stage"
rm -rf "$out"
mkdir -p "$stage"

# Display PCs are linux/amd64. scripts/local-test.sh overrides GOOS/GOARCH to
# run the agent on a Mac, and passes CMS_AGENT_LOCAL_TEST_KEY; CI sets neither.
ldflags="-s -w -X github.com/joshtmags/cms-agent/internal/version.Version=$version"
if [ -n "${CMS_AGENT_LOCAL_TEST_KEY:-}" ]; then
	echo "WARNING: building with a LOCAL TEST signing key (not for release)" >&2
	ldflags="$ldflags -X 'github.com/joshtmags/cms-agent/keys.localTestKey=$CMS_AGENT_LOCAL_TEST_KEY'"
fi
GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}" CGO_ENABLED=0 go build -trimpath \
	-ldflags "$ldflags" \
	-o "$stage/cms-arena-agent" ./cmd/cms-arena-agent

cp bundle/provision.sh "$stage/provision.sh"
cp -R bundle/files "$stage/files"
chmod 0755 "$stage/cms-arena-agent" "$stage/provision.sh"

go run ./cmd/bundletool build --version "$version" --stage "$stage" --out "$out" "$@"
rm -rf "$stage"
ls -l "$out"
