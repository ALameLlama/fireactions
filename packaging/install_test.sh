#!/usr/bin/env bash
set -Eeuo pipefail

source "$(dirname -- "${BASH_SOURCE[0]}")/../install.sh"

temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
fixture="$temp/fixture"
printf 'trusted release artifact\n' > "$fixture"
checksum=$(sha256sum "$fixture")
checksum=${checksum%% *}

curl() { cp "$fixture" "$4"; }
download_checked https://example.invalid/artifact "$checksum" "$temp/download" >/dev/null
cmp "$fixture" "$temp/download"

printf 'changed release artifact\n' > "$fixture"
if download_checked https://example.invalid/artifact "$checksum" "$temp/download" >/dev/null 2>&1; then
  fail "download_checked_rejects_changed_artifact failed: the checksum mismatch was accepted"
fi

printf 'trusted release artifact\n' > "$temp/download"
curl() { return 22; }
if download_checked https://example.invalid/artifact "$checksum" "$temp/download" >/dev/null 2>&1; then
  fail "download_checked_rejects_failed_download failed: a stale matching artifact was accepted"
fi

printf 'Installer download checks passed.\n'
