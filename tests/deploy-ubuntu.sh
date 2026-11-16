#!/usr/bin/env bash
# Run only inside a disposable Ubuntu container with the checkout mounted at /src.
set -euo pipefail
[[ -f /.dockerenv ]] || { printf 'Run this test in a disposable Docker container.\n' >&2; exit 1; }
grep -q '^ID=ubuntu$' /etc/os-release

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends systemd shellcheck curl ca-certificates >/dev/null
shellcheck /src/deploy.sh /src/scripts/build-release.sh

archive=${1:-/src/dist/he-mod-index-v0.1.0-linux-amd64.tar.gz}
work=$(mktemp -d)
tar -xzf "$archive" -C "$work"
package=$(find "$work" -mindepth 1 -maxdepth 1 -type d)
[[ -f $package/he-mod-index.service && -x $package/deploy.sh && -x $package/he-mod-index ]]
mkdir -p /run/systemd/system /run/he-mod-index-test
# Test real Ubuntu users, permissions and binaries without starting a host init.
cat > /usr/local/bin/systemctl <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> /run/he-mod-index-test/systemctl.log
case $1 in
    is-active) [[ -f /run/he-mod-index-test/active ]] ;;
    stop) rm -f /run/he-mod-index-test/active ;;
    restart) touch /run/he-mod-index-test/active ;;
    enable) touch /run/he-mod-index-test/enabled ;;
    daemon-reload) : ;;
    *) printf 'Unexpected systemctl command.\n' >&2; exit 1 ;;
esac
EOF
chmod +x /usr/local/bin/systemctl
bash "$package/deploy.sh" mods.example.org > /tmp/deploy-output
grep -q 'mods.example.org {' /tmp/deploy-output
grep -q 'https://caddyserver.com/docs/install#debian-ubuntu-raspbian' /tmp/deploy-output
[[ -f /run/he-mod-index-test/enabled && -f /run/he-mod-index-test/active ]]
[[ $(stat -c '%U:%G %a' /srv/he-mod-index/he-mod-index) == 'root:root 755' ]]
[[ $(stat -c '%U:%G %a' /srv/he-mod-index/data) == 'he-mod-index:he-mod-index 700' ]]
[[ $(stat -c '%U:%G %a' /etc/he-mod-index.env) == 'root:root 600' ]]
[[ $(getent passwd he-mod-index | cut -d: -f7) == /usr/sbin/nologin ]]
systemd-analyze verify /etc/systemd/system/he-mod-index.service

runuser -u he-mod-index -- env DATA_DIR=/srv/he-mod-index/data ACCOUNT_PASSWORD=deployment-test-password /srv/he-mod-index/he-mod-index user --name test_operator --role admin
runuser -u he-mod-index -- env DATA_DIR=/srv/he-mod-index/data PUBLIC_URL=https://mods.example.org /srv/he-mod-index/he-mod-index > /tmp/app-output 2>&1 &
app_pid=$!
trap 'kill "$app_pid" 2>/dev/null || true' EXIT
for attempt in {1..50}; do
    if curl --silent --fail http://127.0.0.1:8080/healthz > /tmp/health; then break; fi
    sleep 0.1
done
grep -q '^ok$' /tmp/health
kill "$app_pid"
wait "$app_pid" || true
trap - EXIT

printf '\nREGISTRATION_COOLDOWN=2h\n' >> /etc/he-mod-index.env
cp /etc/he-mod-index.env /tmp/config-before
database_before=$(sha256sum /srv/he-mod-index/data/index.db)
printf '\n' >> "$package/he-mod-index"
bash "$package/deploy.sh" > /tmp/upgrade-output
cmp /etc/he-mod-index.env /tmp/config-before
[[ $(sha256sum /srv/he-mod-index/data/index.db) == "$database_before" ]]
cmp "$package/he-mod-index" /srv/he-mod-index/he-mod-index
grep -q '^stop he-mod-index.service$' /run/he-mod-index-test/systemctl.log

# A bad new executable must leave the installed binary and active service alone.
cp /srv/he-mod-index/he-mod-index /tmp/binary-before
cp /run/he-mod-index-test/systemctl.log /tmp/systemctl-before
printf '#!/bin/sh\nexit 1\n' > "$package/he-mod-index"
if bash "$package/deploy.sh" > /tmp/bad-upgrade-output 2>&1; then
    printf 'Bad executable was accepted.\n' >&2; exit 1
fi
cmp /srv/he-mod-index/he-mod-index /tmp/binary-before
cmp /run/he-mod-index-test/systemctl.log /tmp/systemctl-before
printf 'Ubuntu checks passed: ShellCheck, unit verification, users, permissions, service commands, unprivileged app startup, preserved upgrades, and failed-upgrade handling.\n'
