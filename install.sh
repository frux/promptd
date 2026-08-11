#!/bin/sh

set -eu

repository="${PROMPTD_REPOSITORY:-frux/promptd}"
version="${PROMPTD_VERSION:-latest}"
setup_action="${PROMPTD_SETUP:-prompt}"
setup_mode="${PROMPTD_MODE:-auto}"

fail() {
    printf 'promptd installer: %s\n' "$*" >&2
    exit 1
}

case "$version" in
    *[!A-Za-z0-9._-]*) fail "invalid PROMPTD_VERSION: $version" ;;
esac

case "$setup_action" in
    prompt|apply|skip) ;;
    *) fail "PROMPTD_SETUP must be prompt, apply, or skip" ;;
esac

case "$setup_mode" in
    auto|user-systemd|system-systemd|portable) ;;
    *) fail "PROMPTD_MODE must be auto, user-systemd, system-systemd, or portable" ;;
esac

operating_system=$(uname -s 2>/dev/null || true)
case "$operating_system" in
    Linux) operating_system=linux ;;
    *) fail "only Linux release binaries are currently supported" ;;
esac

machine=$(uname -m 2>/dev/null || true)
case "$machine" in
    x86_64|amd64) architecture=amd64 ;;
    aarch64|arm64) architecture=arm64 ;;
    *) fail "unsupported architecture: $machine" ;;
esac

if [ -n "${PROMPTD_INSTALL_DIR:-}" ]; then
    install_directory=$PROMPTD_INSTALL_DIR
else
    [ -n "${HOME:-}" ] || fail "HOME is not set; set PROMPTD_INSTALL_DIR explicitly"
    install_directory=$HOME/.local/bin
fi

asset="promptd_${operating_system}_${architecture}.tar.gz"
release_root="${PROMPTD_RELEASE_BASE_URL:-https://github.com/$repository/releases}"
if [ "$version" = latest ]; then
    release_url="$release_root/latest/download"
else
    release_url="$release_root/download/$version"
fi
case "$release_url" in
    https://*) ;;
    *) fail "release URL must use HTTPS" ;;
esac

temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/promptd-install.XXXXXX") || fail "cannot create a temporary directory"
cleanup() {
    rm -rf "$temporary_directory"
}
trap cleanup EXIT HUP INT TERM

download() {
    source_url=$1
    destination=$2
    if command -v curl >/dev/null 2>&1; then
        curl --proto '=https' --tlsv1.2 -fsSL "$source_url" -o "$destination"
    elif command -v wget >/dev/null 2>&1; then
        wget --https-only -q "$source_url" -O "$destination"
    else
        fail "curl or wget is required"
    fi
}

printf 'Downloading %s...\n' "$asset"
download "$release_url/$asset" "$temporary_directory/$asset" || fail "cannot download $asset"
download "$release_url/$asset.sha256" "$temporary_directory/$asset.sha256" || fail "cannot download checksum"

expected_checksum=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$temporary_directory/$asset.sha256" | tr 'A-F' 'a-f')
[ "${#expected_checksum}" -eq 64 ] || fail "release checksum is missing or malformed"

if command -v sha256sum >/dev/null 2>&1; then
    actual_checksum=$(sha256sum "$temporary_directory/$asset" | awk '{ print $1 }' | tr 'A-F' 'a-f')
elif command -v shasum >/dev/null 2>&1; then
    actual_checksum=$(shasum -a 256 "$temporary_directory/$asset" | awk '{ print $1 }' | tr 'A-F' 'a-f')
else
    fail "sha256sum or shasum is required"
fi
[ "$actual_checksum" = "$expected_checksum" ] || fail "checksum verification failed"

tar -xzf "$temporary_directory/$asset" -C "$temporary_directory" || fail "cannot extract $asset"
[ -f "$temporary_directory/promptd" ] || fail "release archive does not contain promptd"

mkdir -p "$install_directory" || fail "cannot create $install_directory"
destination="$install_directory/promptd"
temporary_destination="$install_directory/.promptd.new.$$"
if command -v install >/dev/null 2>&1; then
    install -m 0755 "$temporary_directory/promptd" "$temporary_destination" || fail "cannot install promptd"
else
    cp "$temporary_directory/promptd" "$temporary_destination" || fail "cannot copy promptd"
    chmod 0755 "$temporary_destination" || fail "cannot make promptd executable"
fi
mv -f "$temporary_destination" "$destination" || fail "cannot replace $destination"

printf 'Installed promptd to %s\n\n' "$destination"

if [ "$setup_action" = skip ]; then
    printf 'Run %s setup when you are ready to configure supervision.\n' "$destination"
    exit 0
fi

"$destination" setup --mode "$setup_mode"

if [ "$setup_action" = apply ]; then
    "$destination" setup --mode "$setup_mode" --apply
    exit 0
fi

if [ -r /dev/tty ]; then
    printf '\nApply this setup plan? [y/N] ' >/dev/tty
    answer=none
    IFS= read -r answer </dev/tty || true
    case "$answer" in
        y|Y|yes|YES|Yes)
            "$destination" setup --mode "$setup_mode" --apply
            ;;
        *)
            printf 'No changes applied. Run %s setup --mode %s --apply later.\n' "$destination" "$setup_mode"
            ;;
    esac
else
    printf '\nNo interactive terminal found. Review the plan, then run:\n  %s setup --mode %s --apply\n' "$destination" "$setup_mode"
fi
