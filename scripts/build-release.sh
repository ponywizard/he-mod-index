#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || ${1:-} == --help || ${1:-} == -h ]]; then
    printf 'Usage: scripts/build-release.sh VERSION [amd64 arm64]\n'
    printf 'Builds Ubuntu/Linux release tarballs and SHA256SUMS in dist/.\n'
    if [[ $# -lt 1 ]]; then exit 1; else exit 0; fi
fi
version=$1
shift
[[ $version =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-+][a-zA-Z0-9.-]+)?$ ]] || { printf 'Invalid release version.\n' >&2; exit 1; }
if (( $# == 0 )); then set -- amd64 arm64; fi
for arch in "$@"; do
    [[ $arch == amd64 || $arch == arm64 ]] || { printf 'Unsupported architecture: %s\n' "$arch" >&2; exit 1; }
done

repo_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
output_dir="$repo_dir/dist"
mkdir -p -- "$output_dir"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
archives=()
for arch in "$@"; do
    name="he-mod-index-${version}-linux-${arch}"
    package_dir="$work_dir/$name"
    mkdir -- "$package_dir"
    (cd -- "$repo_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "$package_dir/he-mod-index" .)
    install -m 0755 -- "$repo_dir/deploy.sh" "$package_dir/deploy.sh"
    install -m 0644 -- "$repo_dir/deploy/he-mod-index.service" "$package_dir/he-mod-index.service"
    install -m 0644 -- "$repo_dir/README.md" "$repo_dir/LICENSE" "$package_dir/"
    tar -czf "$output_dir/$name.tar.gz" -C "$work_dir" -- "$name"
    archives+=("$name.tar.gz")
    printf 'Built %s\n' "$output_dir/$name.tar.gz"
done
(cd -- "$output_dir" && sha256sum -- "${archives[@]}" > SHA256SUMS)
printf 'Checksums: %s/SHA256SUMS\n' "$output_dir"
