#!/usr/bin/env bash
# Runs the agent on this Mac against the local CMS (Laravel Herd, cms.test),
# end to end: mTLS, signed releases, self-update, rollback. Everything it
# creates lives in .local-test/ (git-ignored) and uses THROWAWAY keys.
#
#   scripts/local-test.sh setup            test signing key and test device CA
#   scripts/local-test.sh herd             write the display.cms.test nginx host (mTLS) into Herd
#   scripts/local-test.sh enroll CODE      enroll this "PC" with a code from: php artisan display:device-new
#   scripts/local-test.sh release 0.1.0    build + sign a release with the test key
#   scripts/local-test.sh install 0.1.0    first install into .local-test/root
#   scripts/local-test.sh run              run the agent like systemd would (restarts it after updates)
#   scripts/local-test.sh tamper 0.3.0     build + sign a release, then corrupt its tarball
set -euo pipefail
cd "$(dirname "$0")/.."

lt="$PWD/.local-test"
herd_nginx="$HOME/Library/Application Support/Herd/config/valet/Nginx"
module="github.com/joshtmags/cms-agent"

test_key_line() { sed -n 2p "$lt/signing/test.pub"; }

case "${1:-}" in
setup)
	mkdir -p "$lt/signing" "$lt/ca" "$lt/releases" "$lt/root"
	if [ ! -f "$lt/signing/test.key" ]; then
		minisign -G -W -p "$lt/signing/test.pub" -s "$lt/signing/test.key" >/dev/null
	fi
	if [ ! -f "$lt/ca/ca.crt" ]; then
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
			-keyout "$lt/ca/ca.key" -out "$lt/ca/ca.crt" -subj "/CN=cms-agent LOCAL TEST device CA" 2>/dev/null
	fi
	echo "Created .local-test/. Next:"
	echo "  1. In the cms .env:"
	echo "       DISPLAY_DEVICES_ENABLED=true"
	echo "       DISPLAY_HOST=display.cms.test"
	echo "       DISPLAY_UPDATE_PUBLIC_KEY=$(test_key_line)"
	echo "       DISPLAY_CA_CERT=$lt/ca/ca.crt"
	echo "       DISPLAY_CA_KEY=$lt/ca/ca.key"
	echo "  2. scripts/local-test.sh herd"
	echo "  3. In ~/Code/cms: php artisan migrate && php artisan display:device-new \"Local test PC\" --test-unit"
	echo "  4. scripts/local-test.sh enroll <code>, then approve in the CMS"
	;;

herd)
	# Clone Herd's cms.test HTTPS server block for display.cms.test and add
	# client-certificate checking. The result is handed to PHP as server
	# params (not headers), the same contract the production nginx follows.
	src="$herd_nginx/cms.test"
	dest="$herd_nginx/display.cms.test"
	[ -f "$src" ] || { echo "no Herd config at $src (is cms.test secured?)" >&2; exit 1; }
	python3 - "$src" "$dest" "$lt/ca/ca.crt" <<'PY'
import re, sys
src, dest, ca = sys.argv[1:]
conf = open(src).read()
blocks = re.findall(r"server \{.*?\n\}\n", conf, re.S)
https = next(b for b in blocks if "listen 127.0.0.1:443 ssl" in b)
https = re.sub(r"server_name [^;]+;", "server_name display.cms.test;", https)
https = https.replace("    ssl_certificate_key", f'    ssl_client_certificate "{ca}";\n    ssl_verify_client on;\n    ssl_certificate_key', 1)
https = https.replace("        fastcgi_param PATH_INFO $fastcgi_path_info;",
    "        fastcgi_param PATH_INFO $fastcgi_path_info;\n"
    "        fastcgi_param SSL_CLIENT_VERIFY $ssl_client_verify;\n"
    "        fastcgi_param SSL_CLIENT_FINGERPRINT $ssl_client_fingerprint;", 1)
open(dest, "w").write("# cms-agent LOCAL TEST: mTLS display host (scripts/local-test.sh herd). Delete to undo.\n" + https)
print(f"wrote {dest}")
PY
	herd restart >/dev/null
	echo "Herd restarted. Check: curl -sS https://display.cms.test/device-api/v1/state  (expect 400: no certificate)"
	;;

enroll)
	code="${2:?usage: local-test.sh enroll XXXX-XXXX}"
	go build -ldflags "-X '$module/keys.localTestKey=$(test_key_line)'" -o "$lt/bin/cms-arena-agent" ./cmd/cms-arena-agent
	"$lt/bin/cms-arena-agent" enroll --server https://cms.test --code "$code" --dir "$lt/enrolled" --root "$lt/root"
	# Faster polling and a shorter health window than on a real PC, to watch updates quickly.
	python3 - "$lt/enrolled/config.json" <<'PY2'
import json, sys
p = sys.argv[1]; c = json.load(open(p))
c.update(poll_interval_seconds=5, health_window_seconds=60)
# On the Mac: commands only say what they'd do, except the screenshot (macOS screencapture).
c["commands"] = {
    "restart_browser": ["echo", "would restart the kiosk browser"],
    "reboot": ["echo", "would reboot"],
    "shutdown": ["echo", "would shut down"],
    "screenshot": ["screencapture", "-x", "-t", "png", "{file}"],
}
json.dump(c, open(p, "w"), indent=2)
PY2
	;;

release | tamper)
	version="${2:?usage: local-test.sh $1 X.Y.Z}"
	CMS_AGENT_LOCAL_TEST_KEY="$(test_key_line)" GOOS="$(go env GOOS)" GOARCH="$(go env GOARCH)" \
		scripts/build-release.sh "v$version" >/dev/null
	out="$lt/releases/$version"
	rm -rf "$out" && mkdir -p "$out"
	cp "dist/$version/manifest.json" "dist/$version/cms-arena-agent-$version.tar.gz" "$out/"
	rm -rf "dist/$version"
	minisign -S -s "$lt/signing/test.key" -m "$out/manifest.json" -x "$out/manifest.json.minisig" \
		-t "cms-arena-agent $version" >/dev/null
	if [ "$1" = tamper ]; then
		# Flip bytes in the middle of the tarball AFTER signing.
		python3 -c "import sys; p=sys.argv[1]; b=bytearray(open(p,'rb').read()); b[len(b)//2]^=0xff; open(p,'wb').write(b)" \
			"$out/cms-arena-agent-$version.tar.gz"
		echo "Built, signed and then CORRUPTED $out"
	else
		echo "Built and signed $out"
	fi
	echo "Import it: cd ~/Code/cms && php artisan display:release-import $out"
	;;

install)
	version="${2:?usage: local-test.sh install X.Y.Z}"
	go build -ldflags "-X '$module/keys.localTestKey=$(test_key_line)'" -o "$lt/bin/cms-arena-agent" ./cmd/cms-arena-agent
	"$lt/bin/cms-arena-agent" apply --dir "$lt/releases/$version" --root "$lt/root"
	;;

run)
	[ -x "$lt/root/current/cms-arena-agent" ] || { echo "install a release first" >&2; exit 1; }
	[ -f "$lt/enrolled/config.json" ] || { echo "enroll first" >&2; exit 1; }
	echo "Running the agent like systemd (Restart=always). Ctrl-C to stop."
	trap 'exit 0' INT
	while true; do
		"$lt/root/current/cms-arena-agent" run --config "$lt/enrolled/config.json" || echo "agent exited with $?"
		# Same as systemd's health-check timer: the PREVIOUS binary decides on rollback.
		if [ -x "$lt/root/previous/cms-arena-agent" ]; then
			"$lt/root/previous/cms-arena-agent" healthcheck --root "$lt/root" || true
		fi
		echo "--- restarting $(readlink "$lt/root/current") ---"
		sleep 1
	done
	;;

*)
	sed -n '2,12p' "$0"
	exit 2
	;;
esac
