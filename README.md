# Linux MQTT metrics agent

Install on Linux amd64 or arm64 as root (requires curl, sha256sum, and systemd):

```sh
curl -fsSL https://raw.githubusercontent.com/ronigooja/stats-linux/main/install.sh | sudo sh
```

The installer prompts through the terminal for broker, username, and a hidden, nonempty password. Enter accepts `ssl://mqtt.z0e.dev:8883` only at the installer broker prompt; `run` still requires a configured broker. The device ID defaults to the hostname, sanitized to ASCII letters, digits, `-`, `_`, and `.`, trimmed of edge hyphens and limited to 64 characters. Machines with the same hostname must use different `--device-id` values.

For non-interactive installation, supply credentials in the root environment (load `STATS_PASSWORD` from a protected secret source, not shell history):

```sh
# In a root shell with STATS_PASSWORD already set:
export STATS_BROKER=ssl://broker.example:8883 STATS_USERNAME=my-user STATS_PASSWORD
curl -fsSL https://raw.githubusercontent.com/ronigooja/stats-linux/main/install.sh | sh -s -- --device-id my-host
```

Inspect the script before executing it when required by your trust policy. The installer verifies the release SHA256, installs `/usr/local/bin/stats-linux`, creates root-owned `/etc/stats-linux/config` with mode `0600`, and enables/starts the systemd unit for boot autostart. Existing config skips all prompts and is preserved unless `install --force` is supplied. Passwords must use `STATS_PASSWORD`, never installer arguments.

After installation, the script waits up to 20 seconds for eight seconds of active service without restarts and checks for error-level entries and failure messages in the journal since installation. Failure prints a password-masked journal tail and exits nonzero. For test sandboxes without real systemd, set `STATS_SKIP_HEALTHCHECK=1` to skip this check.

Build with `go build -o stats-linux .`; test with `go test ./...` and `go vet ./...`. Release builds inject the version using `-ldflags "-X main.version=v1.2.3"`; otherwise `version` reports `dev`.

When running the agent, precedence is flags > environment > `/etc/stats-linux/config` > defaults. No subcommand and `run` are equivalent. The config uses EnvironmentFile-style `KEY=VALUE` assignments, blank lines, and full-line `#`/`;` comments. Values may be unquoted, single-quoted, or double-quoted; quote values containing quotes/backslashes. Double quotes support escaped `"`, `\`, `$`, and backticks. Shell expansion, multiline values, and inline comments are not supported. systemd reads this root-only file before dropping privileges.

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-broker` | `STATS_BROKER` | required | TLS MQTT URL, e.g. `ssl://broker.example:8883` |
| `-device-id` | `STATS_DEVICE_ID` | sanitized hostname | Unique topic segment and client ID suffix |
| `-username` | `STATS_USERNAME` | empty | Broker username |
| `-password` | `STATS_PASSWORD` | empty | Broker password; prefer environment over a command-line flag |
| `-ca-file` | `STATS_CA_FILE` | system roots | Additional PEM CA certificates |
| `--disk-path` | `STATS_DISK_PATH` | auto | Comma-separated paths instead of automatic filesystem selection |
| `--interval` | `STATS_INTERVAL` | `1m` | Go duration between samples, e.g. `1m`; minimum `5s` |

TLS certificate and hostname verification are mandatory. Protect environment files with restrictive permissions; environment variables can also be visible to privileged users. Never embed credentials in the broker URL. The broker must support MQTT 3.1.1 and allow retained QoS 1 publishes and a last will. Each agent needs a unique device ID.

All messages are retained JSON under `stats/v1/machines/<device-id>/`: `info` has `hostname` and `platform`; `status` has `online` (false on graceful exit or last will); `metrics` has `version` (1), UTC RFC3339 `timestamp`, `cpu_percent`, `memory` and `disk` (`used_bytes`, `total_bytes`), `network` (`rx_bytes_per_second`, `tx_bytes_per_second`), `load_1m`, and `uptime_seconds`. Network rates use aggregate interface counters and are zero for the first sample or after counter reset. CPU usage is sampled by gopsutil. Metrics are not published when collection fails; retained metrics can therefore be stale. On reconnection, info and online status are republished.

When a publish fails or its acknowledgement times out, the agent exits instead of adding further QoS 1 packets to Paho's reconnect store; systemd restarts it. During disconnection, metrics samples are dropped. On shutdown, MQTT DISCONNECT is sent only after offline status is acknowledged; otherwise process exit leaves the broker's last will armed. Broker detection of an unclean exit can take up to its keepalive timeout. Connection callbacks only notify the publishing loop, so they cannot publish online after the shutdown offline message.

`disk` sums used/total bytes across physical filesystems returned by gopsutil, excluding pseudo/container filesystems such as tmpfs, overlay, squashfs, proc, sysfs, and cgroup. The partition list for automatic selection or explicit-path device resolution is refreshed every 5 minutes (sooner after a filesystem access failure); usage is refreshed every collection. Bind mounts and btrfs subvolumes on the same device count once. Explicit paths bypass automatic filtering but remain device-de-duplicated. Inaccessible, failed, or zero-capacity filesystems are skipped; an empty selection reports zero capacity. The service sandbox may hide mounts, especially home directories and private temporary mounts.

When filesystems are counted, `metrics` also includes `disks`: an array of objects containing only `mount`, `used_bytes`, and `total_bytes`, from the same collection pass as `disk`. Entries are sorted by mount path and limited to the first 32; `disk` still sums all counted filesystems. With `--disk-path`, `mount` contains the selected explicit path after device deduplication. When no filesystems are counted, `disks` is omitted. Mount paths are published to the MQTT broker, so broker readers can see them.

```sh
stats-linux version
stats-linux update --check
sudo stats-linux update
sudo stats-linux restart
sudo stats-linux uninstall          # preserves /etc/stats-linux
sudo stats-linux uninstall --purge  # also removes configuration
```

Updates query the latest GitHub release, select the native amd64/arm64 asset, verify `sha256sums.txt`, and atomically replace the running binary before restarting an active service. Downgrades and updates from unknown (`dev`) versions require `--force`. A failed restart is reported, but does not roll back the binary. Checksums protect integrity, not authenticity against a compromised release account; downloads rely on HTTPS and GitHub trust.

The service uses an unprivileged dynamic user with no capabilities. Its command line never includes credentials. Custom CA files must be service-readable and outside protected home directories. Keep the config root-owned with mode `0600`; after changing it, run `sudo systemctl restart stats-linux`. Release automation builds over SSH on the dedicated Linux machine, requiring repository secrets `BUILD_SSH_KEY` and `BUILD_KNOWN_HOSTS`; it uploads only the two binaries and checksums, with no workflow artifacts or caches. Workflow logs use the repository retention setting.
