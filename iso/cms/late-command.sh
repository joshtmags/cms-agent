#!/bin/sh
# preseed/late_command on the install USB: runs in the installer after the packages are in, with the
# new system at /target. Installs the signed agent release from the stick exactly as a running PC
# does (`cms-arena-agent apply`: checks the signature, runs provision.sh), points apt at the online
# Debian and Tailscale repos for updates, and leaves the PC ready for `sudo cms-pc-setup`.
set -eu

src=/cdrom/simple-cdd
t=/target
# shellcheck source=/dev/null # written by iso/builder.sh
. "$src/image.conf" # IMAGE_NAME, SERVER, AGENT_VERSION
log() { logger -t cms-arena-install "$*"; }

# --- the agent: verified and installed by its own apply ------------------------------------------
mkdir -p "$t/var/tmp/cms-arena-release"
cp "$src"/manifest.json "$src"/manifest.json.minisig "$src"/cms-arena-agent-*.tar.gz "$t/var/tmp/cms-arena-release/"
in-target sh -c 'cd /var/tmp/cms-arena-release && tar -xzf cms-arena-agent-*.tar.gz cms-arena-agent &&
	./cms-arena-agent apply --dir . --root /opt/cms-arena-agent'
rm -rf "$t/var/tmp/cms-arena-release"
log "agent $AGENT_VERSION installed"

# --- apt: online sources for updates (the install itself used only the stick) ------------------------
cat >"$t/etc/apt/sources.list.d/debian.sources" <<'SOURCES'
Types: deb
URIs: http://deb.debian.org/debian
Suites: trixie trixie-updates
Components: main non-free-firmware
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://security.debian.org/debian-security
Suites: trixie-security
Components: main non-free-firmware
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
SOURCES
echo "# Replaced by sources.list.d/debian.sources (install USB)" >"$t/etc/apt/sources.list"
cp "$src/tailscale-archive-keyring.gpg" "$t/usr/share/keyrings/tailscale-archive-keyring.gpg"
cp "$src/tailscale.list" "$t/etc/apt/sources.list.d/tailscale.list"
chmod 0644 "$t/usr/share/keyrings/tailscale-archive-keyring.gpg" "$t/etc/apt/sources.list.d/tailscale.list"

# --- for cms-pc-setup: the CMS address, and the login whose throwaway password it locks ------------------
mkdir -p "$t/etc/cms-arena-agent"
chmod 0700 "$t/etc/cms-arena-agent"
printf 'SETUP_SERVER=%s\n' "$SERVER" >"$t/etc/cms-arena-agent/setup.conf"
chmod 0600 "$t/etc/cms-arena-agent/setup.conf"
echo arena >"$t/etc/cms-arena-agent/install-user"
printf 'IMAGE_NAME=%s\nAGENT_VERSION=%s\nINSTALLED=%s\n' "$IMAGE_NAME" "$AGENT_VERSION" "$(date -u '+%Y-%m-%d %H:%M UTC')" \
	>"$t/etc/cms-arena-image"

# What the console shows before and after logging in.
cat >"$t/etc/issue" <<ISSUE
CMS arena display PC, not set up yet (\\n, \\l)
Log in as arena, then: sudo cms-pc-setup --code XXXX-XXXX

ISSUE
cat >"$t/etc/motd" <<MOTD

  Not set up yet. Get a one-time code in the CMS (Backstage Management -> Display Devices ->
  Add device), then run:

      sudo cms-pc-setup --code XXXX-XXXX

  CMS: $SERVER (change with --server https://...)

MOTD
log "ready for cms-pc-setup ($IMAGE_NAME)"
