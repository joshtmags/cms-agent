#!/bin/sh
# Applies this release's configuration to the display PC. Runs as root:
#   - by the agent after it verified a signed update (before switching to it),
#   - by the agent again on rollback, for the release it returns to,
#   - by `cms-arena-agent apply` (which cms-pc-setup runs) on a fresh install.
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

# Only a Debian display PC gets the rest. Elsewhere (the Mac in scripts/local-test.sh) this is
# where it stops.
if ! [ -f /etc/debian_version ] || ! command -v systemctl >/dev/null 2>&1; then
	log "not a Debian PC with systemd: skipping the PC setup"
	exit 0
fi

daemon_reload=""
changed() { daemon_reload=1; log "updated $1"; }

# --- packages ------------------------------------------------------------------
# files/packages.txt (shared with the install USB). Installing something already there is a
# no-op; waits for an unattended upgrade that holds the apt lock instead of failing.
packages="$(sed 's/#.*//' "$files/packages.txt")"
missing=""
for p in $packages; do
	dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed" || missing="$missing $p"
done
if [ -n "$missing" ]; then
	log "installing:$missing"
	DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 update -qq
	# shellcheck disable=SC2086 # word splitting is the point
	DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y -qq --no-install-recommends $missing
fi

# --- the agent's own units -----------------------------------------------------
for unit in cms-arena-agent.service cms-arena-agent-rollback.service \
	cms-arena-agent-healthcheck.service cms-arena-agent-healthcheck.timer; do
	[ -z "$(install_file "$files/systemd/$unit" "/etc/systemd/system/$unit" 0644)" ] || changed "$unit"
done

# --- helpers the agent's commands and cms-pc-setup call --------------------------
for helper in cms-arena-screenshot cms-arena-kiosk-browser cms-arena-kiosk-cert; do
	[ -z "$(install_file "$files/bin/$helper" "/usr/local/bin/$helper" 0755)" ] || log "updated $helper"
done
# root-only, so it goes in sbin; running it again checks and repairs the PC
[ -z "$(install_file "$files/bin/cms-pc-setup" /usr/local/sbin/cms-pc-setup 0755)" ] || log "updated cms-pc-setup"

# --- kiosk: user, browser policy, unit ----------------------------------------------
if ! id kiosk >/dev/null 2>&1; then
	useradd --create-home --shell /usr/sbin/nologin --comment "CMS arena display kiosk" kiosk
	log "created user kiosk"
fi
usermod -a -G video,render,input kiosk
[ -z "$(install_file "$files/chromium/cms-arena.json" /etc/chromium/policies/managed/cms-arena.json 0644)" ] ||
	log "updated the Chromium policy"
[ -z "$(install_file "$files/systemd/cms-arena-kiosk.service" /etc/systemd/system/cms-arena-kiosk.service 0644)" ] ||
	changed cms-arena-kiosk.service

# --- the PC around it: power button, updates, lockdown ---------------------------
[ -z "$(install_file "$files/etc/logind/cms-arena.conf" /etc/systemd/logind.conf.d/cms-arena.conf 0644)" ] ||
	log "updated the power button (takes effect at the next boot)"
[ -z "$(install_file "$files/etc/apt/20cms-arena-auto-upgrades" /etc/apt/apt.conf.d/20auto-upgrades 0644)" ] ||
	log "updated automatic security updates"
for unit in apt-daily.service apt-daily-upgrade.service; do
	[ -z "$(install_file "$files/etc/systemd/apt-idle.conf" "/etc/systemd/system/$unit.d/cms-arena-idle.conf" 0644)" ] ||
		changed "$unit (idle priority)"
done
if [ -n "$(install_file "$files/etc/sysctl/90-cms-arena.conf" /etc/sysctl.d/90-cms-arena.conf 0644)" ]; then
	sysctl --quiet --load /etc/sysctl.d/90-cms-arena.conf || true
	log "updated sysctl (SysRq off)"
fi
[ -z "$(install_file "$files/etc/modprobe/cms-arena.conf" /etc/modprobe.d/cms-arena.conf 0644)" ] ||
	log "updated the USB storage block (takes effect at the next boot)"

# --- switch it all on ----------------------------------------------------------------
if [ -n "$daemon_reload" ]; then
	systemctl daemon-reload
fi
systemctl set-default --quiet graphical.target
systemctl enable --quiet cms-arena-agent.service cms-arena-agent-healthcheck.timer
# The kiosk takes over tty1: only on an enrolled PC. Before that tty1 is a normal login for
# cms-pc-setup, which switches the kiosk on after enrolling.
if [ -f /etc/cms-arena-agent/config.json ]; then
	systemctl enable --quiet cms-arena-kiosk.service
fi
systemctl start --quiet cms-arena-agent-healthcheck.timer

log "done"
