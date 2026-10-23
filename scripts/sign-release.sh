#!/usr/bin/env bash
# Signs a draft GitHub release on the owner's Mac and publishes it.
# This is the approval step: nothing reaches a PC without it.
#
#   scripts/sign-release.sh v1.2.3
#
# The minisign secret key is read from $CMS_ARENA_SIGNING_KEY
# (default ~/.minisign/cms-arena-agent.key). Keep the key and its passphrase
# in the password manager; copy the key file here only while signing.
set -euo pipefail

tag="${1:?usage: sign-release.sh vX.Y.Z}"
version="${tag#v}"
key="${CMS_ARENA_SIGNING_KEY:-$HOME/.minisign/cms-arena-agent.key}"
cd "$(dirname "$0")/.."

for cmd in gh go minisign; do
	command -v "$cmd" >/dev/null || { echo "missing: $cmd" >&2; exit 1; }
done
[ -f "$key" ] || { echo "signing key not found: $key (set CMS_ARENA_SIGNING_KEY)" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "==> Downloading draft release $tag"
gh release download "$tag" --dir "$work" \
	--pattern manifest.json --pattern "cms-arena-agent-$version.tar.gz"

echo "==> Checking what CI built"
go run ./cmd/bundletool check --version "$version" --dir "$work"

read -r -p "Sign and publish cms-arena-agent $version? [y/N] " answer
[ "$answer" = "y" ] || { echo "not signed"; exit 1; }

echo "==> Signing (minisign asks for the key's passphrase)"
minisign -S -s "$key" -m "$work/manifest.json" -x "$work/manifest.json.minisig" \
	-t "cms-arena-agent $version"

echo "==> Verifying exactly as a display PC will"
go run ./cmd/cms-arena-agent verify --dir "$work" --pubkey keys/update.pub

echo "==> Publishing"
gh release upload "$tag" "$work/manifest.json.minisig"
gh release edit "$tag" --draft=false
echo "Published $tag. Import it in the CMS (Agent releases) and roll it out to the test PC first."
