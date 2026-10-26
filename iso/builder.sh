#!/usr/bin/env bash
# Runs inside the builder container (iso/build-iso.sh starts it):
#   /src      this repo's iso/ (read-only)
#   /release  the signed agent release (already checked)
#   /out      dist/iso/ on the Mac
#   /build/tmp  simple-cdd's package mirror, kept in a Docker volume so a rebuild (another server
#               address) doesn't download everything again
set -euo pipefail
: "${SERVER:?}" "${PASSWORD:?}" "${NAME:?}" "${AGENT_VERSION:?}"
PASSWORD_HASH="$(openssl passwd -6 "$PASSWORD")"
cd /build
rm -rf profiles cms local images
mkdir -p profiles cms local

# --- the agent release, and its package list --------------------------------------------------
# simple-cdd copies extras flat into /simple-cdd/ on the stick: no subdirectories.
cp /release/* cms/
tar -xzf cms/cms-arena-agent-*.tar.gz -C /tmp files/packages.txt
packages="$(sed 's/#.*//' /tmp/files/packages.txt | xargs)"

# --- Tailscale: not in Debian, from its own signed apt repo --------------------------------------
curl -fsSL https://pkgs.tailscale.com/stable/debian/trixie.noarmor.gpg -o cms/tailscale-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian trixie main" \
	>cms/tailscale.list
cp cms/tailscale-archive-keyring.gpg /usr/share/keyrings/
cp cms/tailscale.list /etc/apt/sources.list.d/
apt-get update -qq -o Dir::Etc::sourcelist=/dev/null -o Dir::Etc::sourceparts=/etc/apt/sources.list.d
chmod 0777 local && (cd local && apt-get download -qq tailscale)

# --- profiles --------------------------------------------------------------------------------------
# simple-cdd's own default profile, with our preseed in place of its default.preseed.
for f in /usr/share/simple-cdd/profiles/default.*; do
	[ "$(basename "$f")" = default.preseed ] || cp "$f" profiles/
done
sed "s|@PASSWORD_HASH@|$PASSWORD_HASH|" /src/profiles/default.preseed >profiles/default.preseed
cp /src/profiles/arena.description /src/profiles/arena.downloads profiles/
# shellcheck disable=SC2086 # one package per line
printf '%s\n' $packages tailscale >profiles/arena.packages
# non-free-firmware (GPU, NIC, microcode) on the stick too. In the profile's .conf because
# simple-cdd resets NONFREE after reading --conf, and only sets it for "non-free", not
# "non-free-firmware".
printf 'export NONFREE=1\nexport NONFREE_COMPONENTS="non-free-firmware"\n' >profiles/arena.conf

# --- what late-command.sh installs from the stick ------------------------------------------------------
cp /src/cms/*.sh cms/
cat >cms/image.conf <<CONF
IMAGE_NAME=$NAME
SERVER=$SERVER
AGENT_VERSION=$AGENT_VERSION
CONF

cat >arena.conf <<CONF
profiles="arena"
auto_profiles="arena"
locale="en_US"
keyboard="us"
debian_mirror="http://deb.debian.org/debian/"
mirror_components="main non-free-firmware"
local_packages="/build/local"
all_extras="$(find /build/cms -type f | sort | xargs)"
export ARCHES="amd64"
export DISKINFO="CMS arena display installer ($NAME)"
export OMIT_MANUAL=1
export OMIT_RELEASE_NOTES=1
export DOJIGDO=0
export MAXCDS=1
# 64-bit UEFI only (no 32-bit UEFI boot files: trixie has no i386 installer)
export DISABLE_UEFI_32=1
# No firmware lookup patterns (they need DEP-11 metadata simple-cdd does not mirror): the PC's
# firmware packages come from packages.txt anyway
export DEP11=0
CONF

build-simple-cdd --force-root --dist trixie --dvd --conf arena.conf --logfile /build/tmp/build.log --quiet ||
	{ echo "simple-cdd failed; last log lines:" >&2; tail -n 60 /build/tmp/build.log >&2; exit 1; }

iso="$(find images -name '*.iso' | head -n1)"
cp "$iso" "/out/$NAME.iso"
(cd /out && sha256sum "$NAME.iso" >"$NAME.iso.sha256")
ls -lh "/out/$NAME.iso"
