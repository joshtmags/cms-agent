# Setting up an arena display PC

First-time setup of a display PC (Intel N150, Debian stable) and running it afterwards. Design and reasoning: the cms repo's `docs/arena-display-devices.md`.

> **Status (2026-10-26).** Built and tested: the agent (enroll, check-in, signed updates, commands), the CMS side (devices board, enrollment, sites/IP groups, `/display`). Written, not yet tried on a real PC (next: a Debian VM): `cms-pc-setup` (the one-shot setup, step 3; steps 4–6 are the same by hand) and the kiosk (`cage` + Chromium on `/display`, local start page) in `provision.sh`. These ship from the first release after 1.0.0. **Not built yet:** the install USB, the TPM-held key.

## 1. The CMS side (super admin)

1. The CMS has the display extension on (cms repo `docs/deployment.md` → Display devices) and `DISPLAY_UPDATE_PUBLIC_KEY` set.
2. The agent release is imported (`RELEASING.md` step 5).
3. The site's tag exists with its static IP(s): Display Devices → **New tag** → add addresses.
4. **Add device** → name (e.g. `Arena 1 TV 2`) → note the **one-time code** (valid 30 min, shown once). For the first PC of an install, tick **Test unit**.

## 2. The PC: BIOS and OS

- BIOS: Restore on AC Power Loss = **Power On**, ErP/deep sleep **off**, TPM (Intel PTT) **on**, Secure Boot **on**, then boot from USB for the install.
- Install Debian from the **install USB** (`INSTALL-USB.md`): unattended and offline, one question (the disk to erase), ~10 min. It leaves the agent installed, the CMS address filled in, and a login `arena` / `arena-setup`; then go to step 3 and run just `sudo cms-pc-setup --code XXXX-XXXX` (no download needed).
- Without the stick: Debian stable netinst from debian.org (minimal: no desktop, root password left empty so your user gets sudo, guided whole disk, no encryption), wired Ethernet, NTP on. Then step 3 as written.
- Afterwards: BIOS boot order SSD only, USB boot off, supervisor password.

## 3. Run the setup (`cms-pc-setup`)

Download the release and check its signature before running anything from it:

```bash
sudo apt-get install -y curl minisign
V=1.1.0; mkdir -p ~/agent-$V && cd ~/agent-$V
for f in manifest.json manifest.json.minisig cms-arena-agent-$V.tar.gz; do
  curl -fsSLO "https://github.com/joshtmags/cms-agent/releases/download/v$V/$f"
done
# The public key from the repo (keys/update.pub, second line):
minisign -Vm manifest.json -P RWQsCiXPeRr2RmEu1xOGmuaW6AOVRu9RO9zRNSkYKWVUdiMrOQpJVwYr
tar -xzf cms-arena-agent-$V.tar.gz
sudo ./files/bin/cms-pc-setup --server https://<cms domain> --code XXXX-XXXX
```

It runs, in order: checks (root, Debian, amd64, the CMS reachable, the clock), security updates, the agent install (`apply`: verifies the release again, then `provision.sh`: packages, kiosk, lockdown), the PC's name (`--hostname`, default `arena-<first 4 of the code>`), Tailscale (approve the link on your phone; `--no-tailscale` to skip), enrollment (compare the fingerprint, **Approve** in the CMS), the certificate into the kiosk browser, start, check. Then lock the BIOS and unplug the keyboard.

### Running it again

`cms-pc-setup` is safe to run again, on purpose or by accident, also on a PC that is showing a live event. After the first run it's installed as `/usr/local/sbin/cms-pc-setup`, and it remembers the CMS address and the name (`/etc/cms-arena-agent/setup.conf`), so a rerun is just `sudo cms-pc-setup`.

- **A run that stopped** (an error, Ctrl-C, a power cut, a lost SSH session): run the same command again. It says where it stopped and continues from there; nothing needs undoing.
- **Stopped while waiting for approval:** the code works only once, but the enrollment is kept (`enroll-pending.json` + `enroll-pending.key`). Running again resumes waiting with the same key and fingerprint, without a code. If the CMS meanwhile revoked the device or issued a new code, it says so: run with the new code.
- **On a PC that's already set up:** it only checks and repairs. It skips the updates (left to unattended-upgrades; `--upgrade` to force), keeps the agent version (updates come from the CMS rollout), re-runs the installed release's `provision.sh` (fixes only what's missing or changed), doesn't rename the PC or rejoin Tailscale, doesn't enroll again, and doesn't restart the agent or the kiosk while they're running. The kiosk certificate step changes nothing if the browser already has the certificate.
- **Two at once** (a second terminal or SSH session): the second one refuses to start.
- **Another CMS:** refused while the PC is enrolled with the first one.

### By hand (what the setup does, for troubleshooting)

```bash
sudo ./cms-arena-agent apply --dir .     # verifies again, installs into /opt/cms-arena-agent, runs provision.sh
```

`apply` refuses anything not signed with the key built into it. Result: `/opt/cms-arena-agent/current → releases/<version>` and the systemd units installed and enabled. `apply` also tries to start the agent, which fails until step 4 writes its config; after 5 tries systemd stops retrying. That's expected, and step 5 clears it.

## 4. Enroll

```bash
sudo /opt/cms-arena-agent/current/cms-arena-agent enroll --server https://<cms domain> --code XXXX-XXXX
```

- It prints **Key fingerprint: AB12 CD34 …** and waits.
- In the CMS the card shows "Waiting for approval" with the same fingerprint → **Approve** only if they match.
- It writes `/etc/cms-arena-agent/{device.key, device.crt, ca.crt, config.json}` (the key never leaves the PC) and puts the certificate into the kiosk browser (`cms-arena-kiosk-cert`).
- Interrupted while waiting? Run the same command again (the `--code` can be left out): it resumes with the same key.
- On a PC that is already enrolled it refuses. To enroll again: revoke, **New code**, then `enroll --force …`; the old certificate keeps working until the new one is approved.

## 5. Start and check

```bash
sudo systemctl reset-failed cms-arena-agent   # clears the failed starts from before enrollment (cms-pc-setup does this)
sudo systemctl restart cms-arena-agent
journalctl -u cms-arena-agent -f              # "polling https://display.<domain> every 15s"
```

On the board: the card turns **Active · PC online**, shows the PC's IP. Drag it onto its site's tag (or ⋮ → Move to tag…). Set the layout by clicking the screen drawing on the card.

**Screen:** the kiosk (`cms-arena-kiosk.service`: cage + Chromium as user `kiosk`) opens `/display` on tty1 with the device certificate. On a PC set up with 1.0.0 (no kiosk yet) "Screen not showing" on the card is expected.

## 6. Tailscale (remote support)

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --ssh --accept-dns=false --advertise-tags=tag:arena-display --hostname=arena-<site>-<short id>
```

Approve the login link. The tailnet policy needs the `tag:arena-display` owner and an `ssh` rule for it (admin console).

## Day to day

| Need | Where |
|---|---|
| Is the TV working? | The card: icon color, PC online/offline, Screen live / problems |
| See the screen | Card ⋮ → Take screenshot |
| Fix a stuck screen | ⋮ → Reload screen → Restart browser → Reboot PC |
| End of the day | **Shut down all** (staff power the PCs on in the morning; they come back by themselves after a power cut) |
| Pause a screen | ⋮ → Halt ("Paused by administrator"); **Resume** brings it straight back |
| Backup internet at a site | Click the site's tag → switch the IP check off for 2/6/12 h, with a reason |

Restart/reboot/shut down ask first while a fight event is live.

## Replacing or re-enrolling a PC

- **Lost, stolen or replaced:** ⋮ → **Revoke** (immediate). For the replacement: ⋮ → **New code** on the same card (keeps name, site, layout), then steps 2–5 on the new PC.
- **Same PC, new key** (e.g. after a disk swap): revoke → New code → `sudo /opt/cms-arena-agent/current/cms-arena-agent enroll --force --server https://<cms domain> --code XXXX-XXXX`. A new key means a new fingerprint to compare. After a disk swap the PC is blank: run the whole setup (step 3) instead.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `400 No required SSL certificate was sent` | No or wrong client certificate (not enrolled, or a browser without the device cert) |
| `403 Unknown or revoked device` | Revoked, or enrolled on another CMS |
| `403 Not allowed from <ip> for <tag>` | The site's IP check: add the address to the tag, or switch the check off temporarily |
| `423` / "Paused by administrator" | Halted in the CMS: Resume |
| Certificate errors right after power-up | The clock: check NTP |
| Card "PC offline" | No check-in for 60 s: power, network, `journalctl -u cms-arena-agent` |
