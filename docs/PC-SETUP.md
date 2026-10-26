# Setting up an arena display PC

First-time setup of a display PC (Intel N150, Debian stable) and running it afterwards. Design and reasoning: the cms repo's `docs/arena-display-devices.md`.

> **Status (2026-10-26).** Built and tested: the agent (enroll, check-in, signed updates, commands), the CMS side (devices board, enrollment, sites/IP groups, `/display`). **Not built yet:** `cms-arena-setup` (the one-shot setup script), the kiosk browser (`cage` + Chromium on `/display`), the local start page, the install USB, the TPM-held key. Until then the steps below are the manual path; `cms-arena-setup` will wrap steps 3–5.

## 1. The CMS side (super admin)

1. The CMS has the display extension on (cms repo `docs/deployment.md` → Display devices) and `DISPLAY_UPDATE_PUBLIC_KEY` set.
2. The agent release is imported (`RELEASING.md` step 5).
3. The site's tag exists with its static IP(s): Display Devices → **New tag** → add addresses.
4. **Add device** → name (e.g. `Arena 1 TV 2`) → note the **one-time code** (valid 30 min, shown once). For the first PC of an install, tick **Test unit**.

## 2. The PC: BIOS and OS

- BIOS: Restore on AC Power Loss = **Power On**, ErP/deep sleep **off**, TPM (Intel PTT) **on**, Secure Boot **on**, then boot from USB for the install.
- Install **Debian stable** (minimal, no desktop), wired Ethernet, NTP on (`timedatectl` shows synchronized). A wrong clock makes certificates look invalid.
- Afterwards: BIOS boot order SSD only, USB boot off, supervisor password.

## 3. Install the agent from a signed release

```bash
sudo apt-get install -y curl minisign
V=1.0.0; mkdir -p ~/agent-$V && cd ~/agent-$V
for f in manifest.json manifest.json.minisig cms-arena-agent-$V.tar.gz; do
  curl -fsSLO "https://github.com/joshtmags/cms-agent/releases/download/v$V/$f"
done
# The public key from the repo (keys/update.pub, second line):
minisign -Vm manifest.json -P RWQsCiXPeRr2RmEu1xOGmuaW6AOVRu9RO9zRNSkYKWVUdiMrOQpJVwYr
tar -xzf cms-arena-agent-$V.tar.gz cms-arena-agent
sudo ./cms-arena-agent apply --dir .     # verifies again, installs into /opt/cms-arena-agent, runs provision.sh
```

`apply` refuses anything not signed with the key built into it. Result: `/opt/cms-arena-agent/current → releases/1.0.0` and the systemd units installed and enabled. `apply` also tries to start the agent, which fails until step 4 writes its config; after 5 tries systemd stops retrying. That's expected, and step 5 clears it.

## 4. Enroll

```bash
sudo /opt/cms-arena-agent/current/cms-arena-agent enroll --server https://<cms domain> --code XXXX-XXXX
```

- It prints **Key fingerprint: AB12 CD34 …** and waits.
- In the CMS the card shows "Waiting for approval" with the same fingerprint → **Approve** only if they match.
- It writes `/etc/cms-arena-agent/{device.key, device.crt, ca.crt, config.json}` (the key never leaves the PC).

## 5. Start and check

```bash
sudo systemctl reset-failed cms-arena-agent   # clears the failed starts from before enrollment
sudo systemctl restart cms-arena-agent
journalctl -u cms-arena-agent -f              # "polling https://display.<domain> every 15s"
```

On the board: the card turns **Active · PC online**, shows the PC's IP. Drag it onto its site's tag (or ⋮ → Move to tag…). Set the layout by clicking the screen drawing on the card.

**Screen:** until the kiosk is built, nothing opens `/display` automatically ("Screen not showing" on the card is expected).

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
- **Same PC, new key** (e.g. after a disk swap): revoke → New code → step 4 (`enroll`) again. A new key means a new fingerprint to compare.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `400 No required SSL certificate was sent` | No or wrong client certificate (not enrolled, or a browser without the device cert) |
| `403 Unknown or revoked device` | Revoked, or enrolled on another CMS |
| `403 Not allowed from <ip> for <tag>` | The site's IP check: add the address to the tag, or switch the check off temporarily |
| `423` / "Paused by administrator" | Halted in the CMS: Resume |
| Certificate errors right after power-up | The clock: check NTP |
| Card "PC offline" | No check-in for 60 s: power, network, `journalctl -u cms-arena-agent` |
