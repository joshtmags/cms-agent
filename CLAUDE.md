# CLAUDE.md

The agent that runs on each arena display PC (Go). Its CMS side and the overall design live in the cms repo (`../cms`, `docs/arena-display-devices.md`).

Read these before building, releasing, signing, rolling out, or setting up a PC:

@docs/RELEASING.md
@docs/PC-SETUP.md

Rules that always apply here:

- **Never sign.** Signing needs the owner's passphrase in a real terminal: give the owner the `scripts/sign-release.sh vX.Y.Z` command and wait. Never read `~/Development/secrets/`, and never ask for or handle the passphrase.
- Commit, push and tag only when asked in that message. Tags are never moved or reused; undo a bad release by releasing the next patch.
- `keys/update.pub` is the real public key. Never replace it with the placeholder or a test key; local test builds override it through `scripts/local-test.sh` only.
- Before a release: `go vet ./... && go test ./...`, and CI green on `main`.
