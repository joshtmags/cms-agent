#!/bin/sh
# partman/early_command on the install USB: chooses the disk to install on. The PC's internal disk
# (NVMe, SATA or eMMC), never the USB stick it's installing from, nor any other USB disk. With no
# such disk it stops the install instead of guessing.
set -eu

# The device the installer media is mounted from (e.g. /dev/sdb1 → sdb).
media="$(sed -n 's|^/dev/\([^ ]*\) /cdrom .*|\1|p' /proc/mounts | head -n1)"
media_disk="$media"
if [ -n "$media" ] && [ -e "/sys/class/block/$media/partition" ]; then
	media_disk="$(basename "$(dirname "$(readlink -f "/sys/class/block/$media")")")"
fi

disk=""
for dev in $(list-devices disk); do
	name="${dev#/dev/}"
	[ "$name" != "$media_disk" ] || continue
	[ "$(cat "/sys/block/$name/removable" 2>/dev/null || echo 0)" != 1 ] || continue
	# USB sticks and USB disks that claim to be fixed
	readlink -f "/sys/block/$name" | grep -q '/usb' && continue
	disk="$dev"
	break
done

if [ -z "$disk" ]; then
	logger -t pick-disk "no internal disk found (media: ${media:-none})"
	exit 1
fi
logger -t pick-disk "installing on $disk (media: ${media:-none})"
debconf-set partman-auto/disk "$disk"
