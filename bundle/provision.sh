#!/bin/sh
# Applies this release's configuration to the display PC. Runs as root:
#   - by the agent after it verified a signed update (before switching to it),
#   - by the agent again on rollback, for the release it returns to,
#   - by cms-arena-setup on a fresh install.
#
# Must be idempotent: running it twice changes nothing the second time.
# Must not restart cms-arena-agent itself (the agent exits after an update and
# systemd starts the new binary).
#
# CMS_ARENA_RELEASE_DIR is this release's directory (releases/<version>).
set -eu

release_dir="${CMS_ARENA_RELEASE_DIR:?}"
files="$release_dir/files"

log() { echo "provision: $*"; }

# install_file SRC DEST MODE: copy only when the content differs.
# Prints "changed" when it wrote the file.
install_file() {
	if [ -f "$2" ] && cmp -s "$1" "$2"; then
		return 0
	fi
	install -D -m "$3" "$1" "$2"
	echo changed
}

# --- systemd units for the agent itself ---------------------------------------
# Skipped where there is no systemd (the Mac in scripts/local-test.sh, containers).
if command -v systemctl >/dev/null 2>&1; then
units_changed=""
for unit in cms-arena-agent.service cms-arena-agent-rollback.service \
	cms-arena-agent-healthcheck.service cms-arena-agent-healthcheck.timer; do
	if [ -n "$(install_file "$files/systemd/$unit" "/etc/systemd/system/$unit" 0644)" ]; then
		units_changed=1
		log "updated $unit"
	fi
done
if [ -n "$units_changed" ]; then
	systemctl daemon-reload
fi
systemctl enable --quiet cms-arena-agent.service cms-arena-agent-healthcheck.timer
systemctl start --quiet cms-arena-agent-healthcheck.timer
else
	log "no systemd here, skipping units"
fi

# --- helpers the agent's commands call ------------------------------------------
if [ -n "$(install_file "$files/bin/cms-arena-screenshot" /usr/local/bin/cms-arena-screenshot 0755)" ]; then
	log "updated cms-arena-screenshot"
fi

# --- packages ------------------------------------------------------------------
# Packages the display needs that weren't on the install USB yet. Installing a
# package that is already present is a no-op.
# apt-get install -y --no-install-recommends <package>...

# --- kiosk, Chromium policy, splash page --------------------------------------
# Added in Phase 4 (see the cms repo, docs/arena-display-devices.md §5.6).

log "done"
