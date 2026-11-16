#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: sudo ./deploy.sh [DOMAIN]

Install or upgrade the mod index on Ubuntu with systemd.
DOMAIN is prompted on first installation when omitted.
Existing configuration and data are preserved on upgrades.

For packaging/tests only, DESTDIR=/absolute/path stages files without changing
system users or services. Normal installations do not set DESTDIR.
EOF
}

die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
(( $# <= 1 )) || { usage >&2; exit 1; }

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
install_root=$(realpath -m -- "${DESTDIR:-/}")
if [[ $install_root == / ]]; then install_root=; fi
app_dir="$install_root/srv/he-mod-index"
data_dir="$app_dir/data"
config_file="$install_root/etc/he-mod-index.env"
unit_dir="$install_root/etc/systemd/system"
binary="$script_dir/he-mod-index"
unit="$script_dir/he-mod-index.service"
if [[ ! -f $unit ]]; then unit="$script_dir/deploy/he-mod-index.service"; fi

[[ -f $binary && -x $binary ]] || die "Missing executable beside deploy.sh: $binary"
[[ -f $unit ]] || die 'Missing he-mod-index.service in the release archive.'
for target in "$app_dir" "$data_dir" "$config_file"; do
    [[ ! -L $target ]] || die "Refusing a symlink at installation path: $target"
done

if [[ -z $install_root ]]; then
    (( EUID == 0 )) || die 'Run this installer with sudo.'
    [[ -d /run/systemd/system ]] || die 'This installer requires Ubuntu running systemd.'
    for command in systemctl useradd groupadd runuser getent install chown; do
        command -v "$command" >/dev/null || die "Missing required command: $command"
    done
fi

# Read only the public origin, without executing an EnvironmentFile as shell code.
existing_origin=
if [[ -f $config_file ]]; then
    existing_origin=$(awk '
        /^[[:space:]]*PUBLIC_URL[[:space:]]*=/ {
            sub(/^[^=]*=[[:space:]]*/, ""); sub(/[[:space:]]*$/, ""); value=$0
        }
        END { print value }
    ' "$config_file")
    if [[ $existing_origin == \"*\" || $existing_origin == \'*\' ]]; then
        existing_origin=${existing_origin:1:${#existing_origin}-2}
    fi
fi
domain=${1:-}
if [[ -z $domain && -n $existing_origin ]]; then domain=${existing_origin#https://}; fi
if [[ -z $domain ]]; then
    [[ -t 0 ]] || die 'Supply your domain: sudo ./deploy.sh mods.example.org'
    read -r -p 'Domain (for example mods.example.org): ' domain || die 'Installation cancelled.'
fi
domain=${domain,,}
domain=${domain#https://}
domain=${domain%/}
[[ ${#domain} -le 253 && $domain == *.* && $domain =~ ^[a-z0-9.-]+$ ]] || die 'Enter a domain such as mods.example.org.'
IFS=. read -r -a labels <<< "$domain"
for label in "${labels[@]}"; do
    [[ ${#label} -ge 1 && ${#label} -le 63 && $label =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || die 'Enter a domain without a port, path, or special characters.'
done
[[ $domain != *. ]] || die 'Use the domain without a trailing dot.'
if [[ -f $config_file && $existing_origin != "https://$domain" ]]; then
    die "Existing PUBLIC_URL does not match https://$domain. Update $config_file deliberately before changing domains."
fi

first_install=true
if [[ -f $config_file || -f $data_dir/index.db ]]; then first_install=false; fi

if [[ -z $install_root ]]; then
    if ! getent group he-mod-index >/dev/null; then groupadd --system he-mod-index; fi
    if ! getent passwd he-mod-index >/dev/null; then
        useradd --system --gid he-mod-index --home-dir /srv/he-mod-index --no-create-home --shell /usr/sbin/nologin he-mod-index
    fi
    [[ $(id -u he-mod-index) != 0 ]] || die 'The he-mod-index service account must not be root.'
fi

install -d -m 0755 -- "$app_dir" "$unit_dir"
if [[ -z $install_root ]]; then chown root:root -- "$app_dir"; fi
[[ ! -L $data_dir ]] || die "Refusing a symlink at installation path: $data_dir"
install -d -m 0700 -- "$data_dir"
temporary_paths=()
cleanup() { if (( ${#temporary_paths[@]} )); then rm -f -- "${temporary_paths[@]}"; fi; }
trap cleanup EXIT
# Rename into place, so a failed copy cannot leave a partial executable or unit.
binary_tmp=$(mktemp "$app_dir/.he-mod-index.XXXXXX")
temporary_paths+=("$binary_tmp")
install -m 0755 -- "$binary" "$binary_tmp"
unit_tmp=$(mktemp "$unit_dir/.he-mod-index.service.XXXXXX")
temporary_paths+=("$unit_tmp")
install -m 0644 -- "$unit" "$unit_tmp"

if [[ -z $install_root ]]; then
    # Reject a corrupt/wrong-architecture binary before stopping a running site.
    runuser -u he-mod-index -- "$binary_tmp" --help >/dev/null || die 'The release executable cannot run on this VPS.'
    if systemctl is-active --quiet he-mod-index.service; then systemctl stop he-mod-index.service; fi
fi
mv -f -- "$binary_tmp" "$app_dir/he-mod-index"
mv -f -- "$unit_tmp" "$unit_dir/he-mod-index.service"

if [[ ! -e $config_file ]]; then
    config_tmp=$(mktemp "$install_root/etc/.he-mod-index.env.XXXXXX")
    temporary_paths+=("$config_tmp")
    cat > "$config_tmp" <<EOF
PUBLIC_URL=https://$domain
LISTEN_ADDR=127.0.0.1:8080
DATA_DIR=/srv/he-mod-index/data
TRUSTED_PROXIES=127.0.0.1/32,::1/128
REGISTRATION_COOLDOWN=1h
EOF
    mv -n -- "$config_tmp" "$config_file"
fi

if [[ -z $install_root ]]; then
    chown root:root -- "$app_dir" "$app_dir/he-mod-index" "$config_file" "$unit_dir/he-mod-index.service"
    chown -R --no-dereference he-mod-index:he-mod-index -- "$data_dir"
    if $first_install && [[ -t 0 ]]; then
        printf '\nCreate the admin account:\n'
        runuser -u he-mod-index -- env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DATA_DIR=/srv/he-mod-index/data /srv/he-mod-index/he-mod-index admin
    fi
    systemctl daemon-reload
    systemctl enable he-mod-index.service
    systemctl restart he-mod-index.service
    systemctl is-active --quiet he-mod-index.service || die 'Service failed to start. Check: journalctl -u he-mod-index -n 50'
    printf '\nService installed, enabled, and started.\n'
else
    printf '\nStaged installation under %s. No users or services were changed.\n' "$install_root"
fi

cat <<EOF
Configuration: /etc/he-mod-index.env
Data: /srv/he-mod-index/data

Manage accounts:
  sudo -u he-mod-index env DATA_DIR=/srv/he-mod-index/data /srv/he-mod-index/he-mod-index admin

Install Caddy:
  https://caddyserver.com/docs/install#debian-ubuntu-raspbian

Add this block to /etc/caddy/Caddyfile:

$domain {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
}

Then validate and reload Caddy:
  sudo caddy validate --config /etc/caddy/Caddyfile
  sudo systemctl reload caddy

Status: sudo systemctl status he-mod-index
Logs: sudo journalctl -u he-mod-index -f
EOF
