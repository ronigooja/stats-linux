#!/bin/sh
set -eu

case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "Unsupported architecture: $(uname -m) (need amd64 or arm64)" >&2; exit 1 ;;
esac
[ "$(uname -s)" = Linux ] || { echo "This installer requires Linux" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { echo "Run this installer as root" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' 0
trap 'exit 1' HUP INT TERM
base=https://github.com/ronigooja/stats-linux/releases/latest/download
asset=stats-linux_linux_$arch
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --max-time 120 --max-filesize 104857600 "$base/$asset" -o "$tmp/$asset"
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --max-time 120 --max-filesize 1048576 "$base/sha256sums.txt" -o "$tmp/sha256sums.txt"
awk -v name="$asset" '$2 == name || $2 == "*" name { print }' "$tmp/sha256sums.txt" > "$tmp/checksum"
[ "$(wc -l < "$tmp/checksum" | tr -d ' ')" = 1 ] || { echo "Missing or duplicate release checksum" >&2; exit 1; }
(cd "$tmp" && sha256sum --check checksum) >/dev/null || { echo "Release checksum verification failed" >&2; exit 1; }
chmod 755 "$tmp/$asset"
# STATS_PASSWORD is inherited from the environment, never added to argv.
"$tmp/$asset" install "$@"
