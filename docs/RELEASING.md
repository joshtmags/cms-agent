# Releasing cms-arena-agent

How a new agent version gets from this repo onto the arena display PCs: build, sign, import, roll out. The whole design (why it's built this way) is in the cms repo: `docs/arena-display-devices.md`.

```
commit + push main ──► CI (tests)
git tag vX.Y.Z + push ──► GitHub Actions: build-release.sh ──► DRAFT release (unsigned)
scripts/sign-release.sh vX.Y.Z (owner's Mac, passphrase) ──► signed + PUBLISHED
CMS: Agent Releases → Import from GitHub ──► rollout: None → Test unit → All devices
each PC: verifies the signature itself, installs between events, rolls back if unhealthy
```

## Who does what

| Step | Who | Notes |
|---|---|---|
| Commit, push, tag | Claude or the owner | Claude only when asked (no auto-commit) |
| Watch CI / the release workflow | Claude or the owner | `gh run watch <id> --exit-status` |
| **Sign** | **Owner only, in a normal Terminal window** | `minisign` prompts for the passphrase; it can't through Claude's `!` shell, and Claude never handles the passphrase |
| Check the published release | Claude or the owner | `gh release view` + `cms-arena-agent verify` |
| Import + rollout | Owner (super admin) in the CMS | Backstage Management → Display Devices → Agent Releases |

## Before the first release on a machine

- `gh auth status` (logged in to `joshtmags`), `go version`, `minisign -v` (`brew install minisign`).
- The signing key at `~/.minisign/cms-arena-agent.key`. If it's missing, the owner restores it from the backup (`~/Development/secrets/cms-agent/` or the password manager). **Claude never reads that folder.**
- `keys/update.pub` is the real public key (`go run ./cmd/bundletool key-check` says "valid").

## Versions

- Semver `MAJOR.MINOR.PATCH`, tag `vX.Y.Z`. A tag is used once; never move or reuse one.
- PCs **refuse anything older** than what they run. To undo a bad release, **roll forward**: revert the change, release the next patch version. (A `--rollback` manifest exists in `bundletool`/`build-release.sh`, but the release workflow doesn't build one; roll forward instead.)
- The version is stamped into the binary at build time; `cms-arena-agent version` prints it.

## Steps

### 1. Commit and push

```bash
cd ~/Code/cms-agent
go vet ./... && go test ./...
git push origin main
gh run watch "$(gh run list --branch main --limit 1 --json databaseId --jq '.[0].databaseId')" --exit-status
```

### 2. Tag: GitHub builds the draft

```bash
git tag -a v1.1.0 -m "cms-arena-agent 1.1.0: <what changed>"
git push origin v1.1.0
gh run watch "$(gh run list --workflow release.yml --limit 1 --json databaseId --jq '.[0].databaseId')" --exit-status
gh release view v1.1.0     # draft, 2 files: manifest.json + cms-arena-agent-1.1.0.tar.gz
```

The workflow runs vet + tests, **refuses to build with the placeholder key**, runs `scripts/build-release.sh` (Linux/amd64 binary + `provision.sh` + `bundle/files/` → tarball + `manifest.json` with every file's SHA-256), and creates the draft.

### 3. Sign and publish (owner, Terminal)

```bash
cd ~/Code/cms-agent && scripts/sign-release.sh v1.1.0
```

It downloads the draft, re-hashes everything and shows the files → type **y** → paste the passphrase → it verifies the signature exactly as a PC does → uploads `manifest.json.minisig` → publishes. Only `manifest.json` is signed; it carries the hashes of everything else.

### 4. Check the published release

```bash
gh release view v1.1.0 --json isDraft,assets --jq '{isDraft, assets: [.assets[].name]}'   # false, 3 files
d=$(mktemp -d) && gh release download v1.1.0 --dir "$d" && go run ./cmd/cms-arena-agent verify --dir "$d"; rm -rf "$d"
```

### 5. Import into the CMS

Backstage Management → Display Devices → **Agent Releases → Import from GitHub** → Import. The CMS checks the signature against `DISPLAY_UPDATE_PUBLIC_KEY` (the second line of `keys/update.pub`) and refuses anything else. Offline alternative on the server: `php artisan display:release-import <dir with the 3 files>`.

A new import starts at rollout **None** (offered to no device).

### 6. Roll out

1. Set the release to **Test unit**. Only devices marked test unit (flask badge on the card) get it.
2. Watch the test PC's card: `Agent 1.0.0 → 1.1.0`, then "1.1.0 installed", then **"1.1.0 healthy"**. Take a screenshot from its menu if in doubt.
3. Set it to **All devices**. Each PC updates on its next check-in (~15 s) **between events**: while a fight event is live it waits ("waiting").

### If something goes wrong

| Card / message | Meaning | What to do |
|---|---|---|
| Import: "Signed with a different key" | CMS has another public key | Fix `DISPLAY_UPDATE_PUBLIC_KEY` (local dev uses the throwaway test key on purpose) |
| Import: "has no manifest.json.minisig" | Not signed yet | Step 3 |
| "x.y.z waiting" | A fight event is live | Nothing: it installs after the event |
| "x.y.z rejected: …" | The PC refused the download (signature, hash, version) | Read the error on hover; it retries after 10 min |
| "x.y.z rolled back" | It installed but didn't report healthy within 10 min (or crash-looped) | The PC is back on the previous version and won't retry this one: fix, release the next patch |
| CI: "Refuse to build with the placeholder signing key" | `keys/update.pub` isn't the real key | Restore it from git history; never commit the placeholder |

## The signing key

- **Secret:** `cms-arena-agent.key` (scrypt-encrypted) + its passphrase. Backups: `~/Development/secrets/cms-agent/` and the password manager. Keep the passphrase in the password manager, not next to the key.
- **Public:** `keys/update.pub`, compiled into every agent, and the same value as the CMS's `DISPLAY_UPDATE_PUBLIC_KEY`.
- Only the holder of key + passphrase can ship code to the PCs. Signing covers whatever is in the tag, so review the commits going into a tag before signing.
- **Lost or leaked:** a new key can only reach the PCs in a release signed by the old one. Without the old key, every PC's agent has to be replaced on site.

## Local testing (throwaway key, no GitHub)

`scripts/local-test.sh` builds and signs releases with a test key into `.local-test/` and runs the agent on the Mac against the local CMS (Herd). It never touches the real key; see the README's local test section.
