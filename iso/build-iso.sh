#!/usr/bin/env bash
# Builds the install USB image for arena display PCs: a Debian stable installer that installs
# unattended and offline (every package is on the stick), with a signed agent release and
# cms-pc-setup on it. Runs in Docker (linux/amd64); the result goes to dist/iso/ (git-ignored:
# the ISO is never committed).
#
#   iso/build-iso.sh --release v1.1.0 --server https://cms.example.com     a published, signed release
#   iso/build-iso.sh --release DIR --server https://cms.example.com        the 3 release files in DIR
#   iso/build-iso.sh --test-release 1.1.0 --server https://cms.test        TEST: this checkout, signed with
#                                                                          the throwaway key (.local-test/)
# Options: --password PW   the throwaway install password (default: arena-setup)
#
# Write it to a USB stick with balenaEtcher, or: sudo dd if=<iso> of=/dev/rdiskN bs=4m
# What it installs and how: docs/INSTALL-USB.md.
set -euo pipefail
cd "$(dirname "$0")/.."

release="" test_version="" server="" password="arena-setup"
while [ $# -gt 0 ]; do
	case "$1" in
	--release) release="${2:?}"; shift 2 ;;
	--test-release) test_version="${2:?}"; shift 2 ;;
	--server) server="${2:?}"; shift 2 ;;
	--password) password="${2:?}"; shift 2 ;;
	*) sed -n '2,15p' "$0" >&2; exit 2 ;;
	esac
done
[ -n "$server" ] || { echo "--server is required (the CMS's normal https address)" >&2; exit 2; }
[[ "$server" == https://* ]] || { echo "--server must start with https://" >&2; exit 2; }
[ -n "$release$test_version" ] || { echo "--release or --test-release is required" >&2; exit 2; }

work="$(mktemp -d "${TMPDIR:-/tmp}/cms-iso.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/release" dist/iso
pubkey="$(sed -n 2p keys/update.pub)"
label=""

if [ -n "$test_version" ]; then
	lt=.local-test
	[ -f "$lt/signing/test.key" ] || { echo "no test key: run scripts/local-test.sh setup first" >&2; exit 1; }
	pubkey="$(sed -n 2p "$lt/signing/test.pub")"
	# linux/amd64 (build-release.sh's default), trusting the test key instead of the real one.
	CMS_AGENT_LOCAL_TEST_KEY="$pubkey" scripts/build-release.sh "v$test_version" >/dev/null
	cp "dist/$test_version/manifest.json" "dist/$test_version/cms-arena-agent-$test_version.tar.gz" "$work/release/"
	rm -rf "dist/$test_version"
	minisign -S -s "$lt/signing/test.key" -m "$work/release/manifest.json" -x "$work/release/manifest.json.minisig" \
		-t "cms-arena-agent $test_version (LOCAL TEST)" >/dev/null
	label="-TEST"
	echo "TEST release $test_version: trusts the throwaway key, so it never accepts real releases. For the VM only."
elif [ -d "$release" ]; then
	cp "$release"/manifest.json "$release"/manifest.json.minisig "$release"/cms-arena-agent-*.tar.gz "$work/release/"
else
	tag="v${release#v}"
	gh release download "$tag" --repo joshtmags/cms-agent --dir "$work/release"
fi
# The PC checks it again when it installs (apply), but don't build an image around a bad release.
minisign -Vm "$work/release/manifest.json" -P "$pubkey" >/dev/null || { echo "release signature check failed" >&2; exit 1; }
version="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$work/release/manifest.json" | head -n1)"
# The stick needs the release's package list, setup script and kiosk (from 1.1.0 on).
contents="$(tar -tzf "$work/release"/cms-arena-agent-*.tar.gz)"
for f in files/packages.txt files/bin/cms-pc-setup files/systemd/cms-arena-kiosk.service; do
	grep -qx "$f" <<<"$contents" || {
		echo "release $version is too old for the install USB: it has no $f (use 1.1.0 or later)" >&2
		exit 1
	}
done

image=cms-arena-iso-builder
docker build --platform linux/amd64 -q -t "$image" iso >/dev/null
name="cms-arena-display-$(date +%Y.%m.%d)-agent-$version$label"
docker run --rm --platform linux/amd64 \
	-v "$PWD/iso:/src:ro" -v "$work/release:/release:ro" -v "$PWD/dist/iso:/out" \
	-v cms-arena-iso-cache:/build/tmp \
	-e SERVER="$server" -e PASSWORD="$password" \
	-e NAME="$name" -e AGENT_VERSION="$version" \
	"$image" bash /src/builder.sh
echo
echo "dist/iso/$name.iso"
[ "$password" != arena-setup ] || echo "Install login: arena / arena-setup (locked by cms-pc-setup once Tailscale works)"
