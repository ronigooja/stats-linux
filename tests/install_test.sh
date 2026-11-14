#!/bin/sh
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' 0
trap 'exit 1' HUP INT TERM
mkdir "$tmp/bin"
# Redirect only the fixed config probe; never touch the machine's /etc.
sed "s|/etc/stats-linux/config|$tmp/config|g" "$root/install.sh" > "$tmp/install.sh"
cat > "$tmp/bin/uname" <<'EOF'
#!/bin/sh
case "$1" in -m) echo x86_64 ;; -s) echo Linux ;; esac
EOF
cat > "$tmp/bin/id" <<'EOF'
#!/bin/sh
echo 0
EOF
cat > "$tmp/bin/curl" <<'EOF'
#!/bin/sh
while [ "$#" -gt 0 ]; do
    case "$1" in -o) shift; output=$1 ;; esac
    shift
done
case "$output" in
    */sha256sums.txt) echo 'fakehash  stats-linux_linux_amd64' > "$output" ;;
    *) cp "$FAKE_BINARY" "$output" ;;
esac
EOF
cat > "$tmp/bin/sha256sum" <<'EOF'
#!/bin/sh
exit "${CHECKSUM_EXIT:-0}"
EOF
cat > "$tmp/fake-binary" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$RECORD.args"
printf '%s\n' "${STATS_BROKER:-}" "${STATS_USERNAME:-}" "${STATS_PASSWORD:-}" "${STATS_DEVICE_ID:-}" > "$RECORD.env"
EOF
cat > "$tmp/bin/systemctl" <<'EOF'
#!/bin/sh
echo "$*" >> "$RECORD.systemctl"
case "$1" in
    show)
        if [ "${HEALTH_MODE:-healthy}" = crash ]; then
            count=$(cat "$RECORD.restarts" 2>/dev/null || echo 0)
            count=$((count + 1))
            echo "$count" > "$RECORD.restarts"
            echo "$count"
        elif [ "${HEALTH_MODE:-healthy}" != unavailable ]; then echo 0; fi ;;
    is-active) exit 0 ;;
esac
EOF
cat > "$tmp/bin/journalctl" <<'EOF'
#!/bin/sh
case " $* " in
    *' -n 10 '*) printf '%s\n' 'MQTT connection failed' 'Password: sandbox-secret' 'STATS_PASSWORD=sandbox-secret' ;;
    *' -p err '*) if [ "${HEALTH_MODE:-healthy}" = journal-error ]; then echo 'MQTT connection failed'; fi ;;
    *) case "${HEALTH_MODE:-healthy}" in journal-error|journal-stderr) echo 'MQTT broker rejected the client' ;; esac ;;
esac
EOF
cat > "$tmp/bin/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$tmp/bin/"* "$tmp/fake-binary"
PATH=$tmp/bin:$PATH
FAKE_BINARY=$tmp/fake-binary
RECORD=$tmp/record
export PATH FAKE_BINARY RECORD
STATS_SKIP_HEALTHCHECK=1; export STATS_SKIP_HEALTHCHECK
unset STATS_BROKER STATS_USERNAME STATS_PASSWORD STATS_DEVICE_ID
passed=0
check() {
    if ! "$@"; then echo "FAIL: assertion in case $case_name" >&2; exit 1; fi
}
run() { sh "$tmp/install.sh" "$@" > "$tmp/output" 2>&1; }
case_name=prompts
printf '%s\n' 'ssl://custom:8883' '' 'alice' '' 'test-only-password' > "$tmp/answers"
STATS_TTY=$tmp/answers; export STATS_TTY
run --device-id test-host --interval 1m
check grep -q '^ssl://custom:8883$' "$RECORD.env"
check grep -q '^alice$' "$RECORD.env"
check grep -q '^test-only-password$' "$RECORD.env"
check grep -q '^--interval$' "$RECORD.args"
check test "$(grep -c 'test-only-password' "$RECORD.args" || :)" = 0
check test "$(grep -c 'test-only-password' "$tmp/output" || :)" = 0
passed=$((passed + 1))
case_name=default
printf '\nalice\ntest-only-password\n' > "$tmp/answers"
run
check grep -q '^ssl://mqtt.z0e.dev:8883$' "$RECORD.env"
passed=$((passed + 1))
case_name=piped-stdin
cat "$tmp/install.sh" | sh > "$tmp/output" 2>&1
check grep -q '^ssl://mqtt.z0e.dev:8883$' "$RECORD.env"
passed=$((passed + 1))
case_name=end-of-input
: > "$tmp/answers"
if run; then echo 'FAIL: end of input accepted' >&2; exit 1; fi
check grep -q STATS_PASSWORD "$tmp/output"
passed=$((passed + 1))
case_name=environment
STATS_BROKER=ssl://env:8883 STATS_USERNAME=env-user STATS_PASSWORD=test-env-password STATS_DEVICE_ID=env-host STATS_TTY=$tmp/absent run
check grep -q '^env-user$' "$RECORD.env"
check test ! -s "$tmp/output"
passed=$((passed + 1))
case_name=flags
unset STATS_BROKER STATS_USERNAME STATS_PASSWORD STATS_DEVICE_ID
STATS_PASSWORD=test-env-password STATS_TTY=$tmp/absent run --broker=ssl://flag:8883 --username flag-user
check test ! -s "$tmp/output"
passed=$((passed + 1))
case_name=missing
unset STATS_BROKER STATS_USERNAME STATS_PASSWORD STATS_DEVICE_ID
if STATS_TTY=$tmp/absent run; then echo 'FAIL: missing input accepted' >&2; exit 1; fi
for name in STATS_BROKER STATS_USERNAME STATS_PASSWORD STATS_DEVICE_ID; do check grep -q "$name" "$tmp/output"; done
passed=$((passed + 1))
case_name=existing
touch "$tmp/config"
STATS_TTY=$tmp/absent run
check grep -q 'Keeping existing' "$tmp/output"
passed=$((passed + 1))
case_name=checksum
if CHECKSUM_EXIT=1 run; then echo 'FAIL: bad checksum accepted' >&2; exit 1; fi
check grep -q 'checksum verification failed' "$tmp/output"
passed=$((passed + 1))
case_name=password-argv
if run --password unsafe-test-value; then echo 'FAIL: password argument accepted' >&2; exit 1; fi
check grep -q STATS_PASSWORD "$tmp/output"
passed=$((passed + 1))
case_name=health-skipped
unset CHECKSUM_EXIT
rm -f "$RECORD.systemctl"
run
check test ! -e "$RECORD.systemctl"
check test "$(grep -c 'stats-linux is running' "$tmp/output" || :)" = 0
passed=$((passed + 1))
STATS_SKIP_HEALTHCHECK=0
case_name=healthy
run
check grep -q 'stats-linux is running' "$tmp/output"
passed=$((passed + 1))
case_name=health-restarts-unavailable
HEALTH_MODE=unavailable run
check grep -q 'stats-linux is running' "$tmp/output"
passed=$((passed + 1))
case_name=health-crash-loop
if HEALTH_MODE=crash run; then echo 'FAIL: crash loop accepted' >&2; exit 1; fi
check grep -q 'MQTT connection failed' "$tmp/output"
check grep -q 'journalctl -u stats-linux' "$tmp/output"
check grep -q '\[REDACTED\]' "$tmp/output"
check test "$(grep -c 'sandbox-secret' "$tmp/output" || :)" = 0
passed=$((passed + 1))
case_name=health-journal-error
if HEALTH_MODE=journal-error run; then echo 'FAIL: journal error accepted' >&2; exit 1; fi
check grep -q 'MQTT connection failed' "$tmp/output"
passed=$((passed + 1))
case_name=health-journal-stderr
if HEALTH_MODE=journal-stderr run; then echo 'FAIL: stderr refusal accepted' >&2; exit 1; fi
check grep -q 'MQTT connection failed' "$tmp/output"
passed=$((passed + 1))
echo "install.sh: $passed cases passed, 0 failed"
