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
echo_disabled=0
cleanup() {
    if [ "$echo_disabled" = 1 ]; then stty echo <&3 2>/dev/null || :; fi
    rm -rf "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

# Inspect flags without consuming them: the original arguments reach install unchanged.
broker=${STATS_BROKER:-}
username=${STATS_USERNAME:-}
next=
for arg do
    case "$next" in
        broker) broker=$arg; next=; continue ;;
        username) username=$arg; next=; continue ;;
    esac
    case "$arg" in
        --broker|-broker) next=broker ;;
        --broker=*|-broker=*) broker=${arg#*=} ;;
        --username|-username) next=username ;;
        --username=*|-username=*) username=${arg#*=} ;;
        --password|-password|--password=*|-password=*)
            echo "Pass the password via STATS_PASSWORD, never a command-line flag" >&2; exit 1 ;;
    esac
done

missing_values() {
    echo "No usable prompt input. Set STATS_BROKER (or --broker), STATS_USERNAME (or --username), and STATS_PASSWORD; optionally set STATS_DEVICE_ID (or --device-id) instead of the hostname default." >&2
    exit 1
}

if [ -f /etc/stats-linux/config ]; then
    echo "Keeping existing /etc/stats-linux/config (unless --force is supplied)."
elif [ -z "$broker" ] || [ -z "$username" ] || [ -z "${STATS_PASSWORD:-}" ]; then
    # stdin may contain this script (curl | sh); never read answers from it.
    tty=${STATS_TTY:-/dev/tty}
    if ! (exec 3< "$tty") 2>/dev/null; then missing_values; fi
    exec 3< "$tty"
    if [ -z "$broker" ]; then
        printf 'Broker [ssl://mqtt.z0e.dev:8883]: ' >&2
        IFS= read -r broker <&3 || missing_values
        STATS_BROKER=${broker:-ssl://mqtt.z0e.dev:8883}
        export STATS_BROKER
    fi
    while [ -z "$username" ]; do
        printf 'Username: ' >&2
        IFS= read -r username <&3 || missing_values
        [ -n "$username" ] || echo "Username is required." >&2
        STATS_USERNAME=$username
        export STATS_USERNAME
    done
    while [ -z "${STATS_PASSWORD:-}" ]; do
        printf 'Password: ' >&2
        if [ -t 3 ]; then
            echo_disabled=1
            stty -echo <&3 || exit 1
        fi
        IFS= read -r STATS_PASSWORD <&3 || missing_values
        if [ "$echo_disabled" = 1 ]; then
            stty echo <&3 || exit 1
            echo_disabled=0
        fi
        printf '\n' >&2
        [ -n "$STATS_PASSWORD" ] || echo "Password is required; use STATS_PASSWORD for non-interactive installation." >&2
    done
fi
base=https://github.com/ronigooja/stats-linux/releases/latest/download
asset=stats-linux_linux_$arch
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --max-time 120 --max-filesize 104857600 "$base/$asset" -o "$tmp/$asset"
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --max-time 120 --max-filesize 1048576 "$base/sha256sums.txt" -o "$tmp/sha256sums.txt"
awk -v name="$asset" '$2 == name || $2 == "*" name { print }' "$tmp/sha256sums.txt" > "$tmp/checksum"
[ "$(wc -l < "$tmp/checksum" | tr -d ' ')" = 1 ] || { echo "Missing or duplicate release checksum" >&2; exit 1; }
(cd "$tmp" && sha256sum --check checksum) >/dev/null || { echo "Release checksum verification failed" >&2; exit 1; }
chmod 755 "$tmp/$asset"
# STATS_PASSWORD is inherited from the environment, never added to argv.
STATS_PASSWORD=${STATS_PASSWORD:-} "$tmp/$asset" install "$@"
