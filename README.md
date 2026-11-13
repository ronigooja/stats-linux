# Linux MQTT metrics agent

Build on Linux with Go 1.21 or later: run `go mod tidy` and then `go build -o stats-linux .`. The first command fetches dependencies from the configured Go module proxy and generates `go.sum`. Run tests with `go test ./...`.

Configure with flags or their environment equivalents (flags take precedence):

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `-broker` | `STATS_BROKER` | required | TLS MQTT URL, e.g. `ssl://broker.example:8883` |
| `-device-id` | `STATS_DEVICE_ID` | required | Unique topic segment and client ID suffix |
| `-username` | `STATS_USERNAME` | empty | Broker username |
| `-password` | `STATS_PASSWORD` | empty | Broker password; prefer environment over a command-line flag |
| `-ca-file` | `STATS_CA_FILE` | system roots | Additional PEM CA certificates |
| `-disk-path` | `STATS_DISK_PATH` | `/` | Filesystem to report |
| `-interval` | `STATS_INTERVAL` | `30s` | Go duration between samples |

TLS certificate and hostname verification are mandatory. Protect environment files with restrictive permissions; environment variables can also be visible to privileged users. Never embed credentials in the broker URL. The broker must support MQTT 3.1.1 and allow retained QoS 1 publishes and a last will. Each agent needs a unique device ID.

All messages are retained JSON under `stats/v1/machines/<device-id>/`: `info` has `hostname` and `platform`; `status` has `online` (false on graceful exit or last will); `metrics` has `version` (1), UTC RFC3339 `timestamp`, `cpu_percent`, `memory` and `disk` (`used_bytes`, `total_bytes`), `network` (`rx_bytes_per_second`, `tx_bytes_per_second`), `load_1m`, and `uptime_seconds`. Network rates use aggregate interface counters and are zero for the first sample or after counter reset. CPU usage is sampled by gopsutil. Metrics are not published when collection fails; retained metrics can therefore be stale. On reconnection, info and online status are republished.

When a publish fails or its acknowledgement times out, the agent exits instead of adding further QoS 1 packets to Paho's reconnect store; systemd restarts it. During disconnection, metrics samples are dropped. On shutdown, MQTT DISCONNECT is sent only after offline status is acknowledged; otherwise process exit leaves the broker's last will armed. Broker detection of an unclean exit can take up to its keepalive timeout. Connection callbacks only notify the publishing loop, so they cannot publish online after the shutdown offline message.

For systemd, install the binary at `/usr/local/bin/stats-linux`, copy `stats-linux.service` to `/etc/systemd/system/`, and create `/etc/stats-linux.env` with permissions `0600`, owned by root. Set `STATS_BROKER`, `STATS_DEVICE_ID`, and any credentials in that file. Reload systemd and enable the service. The service uses an unprivileged dynamic user with no capabilities; systemd reads the root-only environment file before dropping privileges. Custom CA files must be readable by the service and outside protected home directories. The default disk path `/` is supported; sandboxing may hide other mount paths.
