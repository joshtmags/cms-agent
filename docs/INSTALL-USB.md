# The install USB

One USB stick sets up any number of display PCs identically. It's a Debian stable installer that installs **unattended and offline** (every package is on the stick), with a signed agent release and `cms-pc-setup` on it. After it, a PC needs only `sudo cms-pc-setup --code XXXX-XXXX` (`PC-SETUP.md`).

Why an installer and not a cloned disk: every PC must create its own secrets (device key, SSH host keys, machine-id, Tailscale identity). The design is in the cms repo: `docs/arena-display-devices.md` → Install USB.

## Build it (Mac, Docker Desktop running)

```bash
cd ~/Code/cms-agent
# A published release (signed by the owner) for a client's CMS:
iso/build-iso.sh --release v1.1.0 --server https://cms.<client domain>
# TEST: this checkout, signed with the throwaway key in .local-test/ (for a VM, never for a real PC):
iso/build-iso.sh --test-release 1.1.0 --server https://display.cms.test
```

- Output: `dist/iso/cms-arena-display-<date>-agent-<version>[-TEST].iso` + `.sha256`. **Never commit it** (`dist/` is git-ignored; ~1–1.5 GB).
- One ISO per CMS install (client): `--server` is pre-filled into the PC's `cms-pc-setup` (`/etc/cms-arena-agent/setup.conf`); `--server` on the PC still overrides it. It's the CMS's **normal** address (enrollment runs there), not the display host.
- The first build downloads every package (~1 GB) into the Docker volume `cms-arena-iso-cache`; later builds (another server address, a new release) reuse it. `docker volume rm cms-arena-iso-cache` to start clean.
- On Apple Silicon the builder runs emulated (linux/amd64): slower, same result.
- A `-TEST` ISO's agent trusts the throwaway key, so it never accepts a real release from the CMS. Rebuild with `--release` before any real PC.

Tested (2026-10-26) in a UEFI QEMU VM: unattended install from the stick, boot, `cms-pc-setup` (enroll against the local CMS, interrupted and resumed without the code, agent "healthy", kiosk on the screen). **Not yet on real hardware:** Secure Boot, the Intel GPU / video decode, the N150's NIC.

What goes in: Debian 13 (trixie) amd64 installer + `bundle/files/packages.txt` from that release (the same list `provision.sh` keeps installed) + Tailscale (from its signed apt repo) + the boot loader for UEFI/Secure Boot + the signed release itself + `iso/cms/*.sh`.

## Write it to a stick (8 GB or more)

balenaEtcher (select the ISO, the stick, Flash), or:

```bash
shasum -a 256 -c dist/iso/<name>.iso.sha256
diskutil list                          # find the stick, e.g. /dev/disk4: CHECK the size
diskutil unmountDisk /dev/disk4
sudo dd if=dist/iso/<name>.iso of=/dev/rdisk4 bs=4m status=progress
diskutil eject /dev/disk4
```

`dd` to the wrong disk erases it: check `diskutil list` twice.

## Install a PC (~10 min)

1. BIOS (`PC-SETUP.md` step 2): Restore on AC Power Loss = Power On, ErP off, TPM on, Secure Boot on, USB boot on. Wired network plugged in (not needed for the install, needed for the setup after it).
2. Boot from the stick (the boot menu key, often F7/F11/F12 on N150 boxes) → **Install** (or Graphical install). It waits at this menu: nothing starts by itself.
3. It runs by itself. If the internal disk already has partitions (Windows from the factory, an earlier install), it asks **one question**: "Write the changes to disks?", naming the disk it will **erase** (the internal NVMe/SSD; it never picks a USB disk, and stops if there's no internal disk). Check it, answer **Yes**. A blank disk has nothing to lose, so it doesn't ask.
4. It installs from the stick, installs and checks the agent, then **powers off**. Take the stick out.
5. Power on. The console says "not set up yet". Log in as **`arena`**, password **`arena-setup`** (throwaway, public: it's in this repo), then:

   ```bash
   sudo cms-pc-setup --code XXXX-XXXX
   ```

   It installs updates released since the ISO was built, names the PC, joins Tailscale, enrolls, and **locks the `arena` password** once Tailscale works (from then on: `tailscale ssh arena@<pc>`, sudo without a password). Its last step switches the console to the display.
6. Lock the BIOS (SSD only, USB boot off, supervisor password), unplug the keyboard.

## What's on an installed PC

| | |
|---|---|
| Disk | One ext4 partition + swap, **not encrypted** yet (encryption comes with the TPM unlock, stage 6: without it every power-on would ask for a passphrase). A stolen PC's device key can be read: **Revoke** it in the CMS. |
| Logins | No root login. `arena`: sudo; throwaway password until `cms-pc-setup` locks it. |
| apt | `deb.debian.org` trixie + trixie-updates + trixie-security (main, non-free-firmware) and `pkgs.tailscale.com`. The stick is no longer used. |
| Agent | `/opt/cms-arena-agent/current` → the release from the stick; the CMS rollout updates it. |
| Kiosk | Installed, switched on by `cms-pc-setup` after enrollment (until then tty1 is a normal login). |
| `/etc/cms-arena-image` | Which ISO installed it, the agent version, when. |

## When something goes wrong

| What you see | What to do |
|---|---|
| "Execution of preseeded command failed" at partitioning | No internal disk found (only USB disks): check the SSD is seated / seen in the BIOS. Alt-F4 shows the installer log (`pick-disk`). |
| "Execution of preseeded command failed" near the end | The late command (agent install): Alt-F4 log, lines `cms-arena-install` / `provision`. A package missing from the stick means `packages.txt` and the ISO don't match: rebuild. |
| The installer asks other questions | Something the preseed doesn't answer on this hardware (e.g. Wi-Fi only, missing firmware). Answer it, and note it: `iso/profiles/default.preseed` gets the answer. |
| Boots to the stick again after the install | Take the stick out (the install powers off for that). |

## Files

| File | What it is |
|---|---|
| `iso/build-iso.sh` | Mac side: picks and checks the release, runs the builder container |
| `iso/Dockerfile`, `iso/builder.sh` | The builder (Debian trixie + simple-cdd) |
| `iso/profiles/default.preseed` | Every installer answer |
| `iso/profiles/arena.downloads` | Boot loader/kernel packages the installer picks itself |
| `iso/cms/pick-disk.sh` | Chooses the internal disk, never a USB one |
| `iso/cms/late-command.sh` | End of the install: `apply` the release, apt sources, `setup.conf`, console text |
