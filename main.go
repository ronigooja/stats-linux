package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

type config struct {
	broker, deviceID, username, password, caFile, diskPath string
	interval                                               time.Duration
}

type capacity struct {
	Used  uint64 `json:"used_bytes"`
	Total uint64 `json:"total_bytes"`
}
type rates struct {
	RX float64 `json:"rx_bytes_per_second"`
	TX float64 `json:"tx_bytes_per_second"`
}
type metrics struct {
	Version   int            `json:"version"`
	Timestamp time.Time      `json:"timestamp"`
	CPU       float64        `json:"cpu_percent"`
	Memory    capacity       `json:"memory"`
	Disk      capacity       `json:"disk"`
	Disks     []diskCapacity `json:"disks,omitempty"`
	Network   rates          `json:"network"`
	Load      float64        `json:"load_1m"`
	Uptime    uint64         `json:"uptime_seconds"`
}
type counters struct {
	rx, tx uint64
	at     time.Time
}

func networkRates(previous, current counters) rates {
	seconds := current.at.Sub(previous.at).Seconds()
	if previous.at.IsZero() || seconds <= 0 || current.rx < previous.rx || current.tx < previous.tx {
		return rates{}
	}
	return rates{float64(current.rx-previous.rx) / seconds, float64(current.tx-previous.tx) / seconds}
}
func collect(ctx context.Context, path string, previous counters, disksCache *diskTargetCache) (metrics, counters, error) {
	var m metrics
	percent, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil || len(percent) == 0 {
		return m, previous, fmt.Errorf("cpu: %v", err)
	}
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return m, previous, fmt.Errorf("memory: %w", err)
	}
	d, disks, err := disksCache.collectDisks(ctx, path)
	if err != nil {
		return m, previous, fmt.Errorf("disk: %w", err)
	}
	n, err := net.IOCountersWithContext(ctx, false)
	if err != nil || len(n) == 0 {
		return m, previous, fmt.Errorf("network: %v", err)
	}
	l, err := load.AvgWithContext(ctx)
	if err != nil {
		return m, previous, fmt.Errorf("load: %w", err)
	}
	u, err := host.UptimeWithContext(ctx)
	if err != nil {
		return m, previous, fmt.Errorf("uptime: %w", err)
	}
	now := time.Now().UTC()
	current := counters{n[0].BytesRecv, n[0].BytesSent, now}
	m = metrics{1, now, percent[0], capacity{vm.Used, vm.Total}, d, disks, networkRates(previous, current), l.Load1, u}
	return m, current, nil
}

type publisher interface {
	IsConnectionOpen() bool
	Publish(string, byte, bool, interface{}) mqtt.Token
	Disconnect(uint)
}

var errOffline = errors.New("MQTT connection is not open")

func publish(client publisher, topic string, value any) error {
	if !client.IsConnectionOpen() {
		return errOffline
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t := client.Publish(topic, 1, true, b)
	if !t.WaitTimeout(10 * time.Second) {
		return errors.New("publish timed out")
	}
	return t.Error()
}

// Only the run loop publishes. A failed/timed-out Publish may still be stored by
// Paho, so callers must exit instead of creating more unresolved QoS 1 packets.
func shutdown(client publisher, base string) error {
	if err := publish(client, base+"status", map[string]bool{"online": false}); err != nil {
		// Do not send MQTT DISCONNECT: process exit closes the socket and leaves
		// the broker's will armed when offline delivery was not acknowledged.
		return fmt.Errorf("publish offline (leaving last will armed): %w", err)
	}
	client.Disconnect(250)
	return nil
}

func announce(client publisher, base, name, platform string) error {
	if err := publish(client, base+"info", map[string]string{"hostname": name, "platform": platform}); err != nil {
		return fmt.Errorf("publish info: %w", err)
	}
	return publish(client, base+"status", map[string]bool{"online": true})
}

// mqttRefusalMessage returns only fixed text: broker errors must never leak credentials.
func mqttRefusalMessage(err error) string {
	for _, refusal := range []struct {
		err     error
		message string
	}{
		{packets.ErrorRefusedBadUsernameOrPassword, "MQTT broker rejected the username or password (check STATS_USERNAME / STATS_PASSWORD)"},
		{packets.ErrorRefusedNotAuthorised, "MQTT broker rejected the client (not authorized)"},
		{packets.ErrorRefusedIDRejected, "MQTT broker rejected the client identifier (check STATS_DEVICE_ID)"},
		{packets.ErrorRefusedServerUnavailable, "MQTT broker is unavailable"},
		{packets.ErrorRefusedBadProtocolVersion, "MQTT broker rejected the protocol version (MQTT 3.1.1 required)"},
		{packets.ErrorProtocolViolation, "MQTT broker rejected the connection (protocol violation)"},
	} {
		if errors.Is(err, refusal.err) {
			return refusal.message
		}
	}
	return ""
}

func connectInitial(ctx context.Context, connect func() mqtt.Token, timeout, retry time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		token := connect()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			// Prefer a refusal already reported at the deadline boundary.
			if message := mqttRefusalMessage(token.Error()); message != "" {
				return errors.New(message)
			}
			return errors.New("MQTT connection timed out")
		case <-token.Done():
		}
		if token.Error() == nil {
			return nil
		}
		if message := mqttRefusalMessage(token.Error()); message != "" {
			return errors.New(message)
		}
		pause := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			pause.Stop()
			return ctx.Err()
		case <-deadline.C:
			pause.Stop()
			return errors.New("MQTT connection timed out")
		case <-pause.C:
		}
	}
}

func brokerTLS(c config) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.caFile != "" {
		pem, err := os.ReadFile(c.caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	return tlsConfig, nil
}

// clearRetained applies only to uninstall: empty bytes delete retained records,
// unlike publish's JSON encoding of nil ("null").
func clearRetained(client publisher, deviceID string) error {
	for _, suffix := range []string{"info", "metrics", "status"} {
		if !client.IsConnectionOpen() {
			return errOffline
		}
		token := client.Publish("stats/v1/machines/"+deviceID+"/"+suffix, 1, true, []byte{})
		if !token.WaitTimeout(10 * time.Second) {
			return errors.New("cleanup publish timed out")
		}
		if err := token.Error(); err != nil {
			return err
		}
	}
	return nil
}

func clearRemote(c config) error {
	tlsConfig, err := brokerTLS(c)
	if err != nil {
		return err
	}
	opts := mqtt.NewClientOptions().AddBroker(c.broker).SetProtocolVersion(4).
		SetClientID("stats-" + c.deviceID + "-cleanup").SetUsername(c.username).SetPassword(c.password).
		SetTLSConfig(tlsConfig).SetAutoReconnect(false).SetConnectRetry(false).
		SetConnectTimeout(10 * time.Second).SetWriteTimeout(5 * time.Second)
	client := mqtt.NewClient(opts)
	defer client.Disconnect(250)
	token := client.Connect()
	if !token.WaitTimeout(10 * time.Second) {
		return errors.New("cleanup connection timed out")
	}
	if err := token.Error(); err != nil {
		return err
	}
	return clearRetained(client, c.deviceID)
}

func run(c config) error {
	tlsConfig, err := brokerTLS(c)
	if err != nil {
		return err
	}
	name, err := os.Hostname()
	if err != nil {
		return err
	}
	hi, err := host.Info()
	if err != nil {
		return fmt.Errorf("host info: %w", err)
	}
	base := "stats/v1/machines/" + c.deviceID + "/"
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connected := make(chan struct{}, 1)
	// Paho v1.5.0 hides initial CONNACK errors with ConnectRetry enabled.
	// Retry initial network failures ourselves; retain automatic reconnect after success.
	opts := mqtt.NewClientOptions().AddBroker(c.broker).SetProtocolVersion(4).SetClientID("stats-"+c.deviceID).SetUsername(c.username).SetPassword(c.password).SetTLSConfig(tlsConfig).SetAutoReconnect(true).SetConnectRetry(false).SetWriteTimeout(5*time.Second).SetWill(base+"status", `{"online":false}`, 1, true)
	opts.SetOnConnectHandler(func(mqtt.Client) {
		// Paho runs callbacks concurrently, including during shutdown. Never
		// publish here; coalesce notifications for the sole publishing loop.
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	client := mqtt.NewClient(opts)
	if err := connectInitial(ctx, client.Connect, 30*time.Second, 5*time.Second); err != nil {
		return err // Exit without DISCONNECT; a concurrent connection may have a will.
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	var prev counters
	disksCache := diskTargetCache{now: time.Now, partitions: disk.PartitionsWithContext}
	for {
		// Cancellation takes priority over queued reconnection notifications.
		if ctx.Err() != nil {
			return shutdown(client, base)
		}
		select {
		case <-connected:
			if err := announce(client, base, name, hi.Platform); err != nil {
				return fmt.Errorf("announce: %w", err)
			}
		default:
		}
		if ctx.Err() != nil {
			return shutdown(client, base)
		}
		m, next, err := collect(ctx, c.diskPath, prev, &disksCache)
		if err != nil {
			log.Printf("collect metrics: %v", err)
		} else {
			prev = next
			if client.IsConnectionOpen() {
				if err := publish(client, base+"metrics", m); err != nil {
					return fmt.Errorf("publish metrics: %w", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return shutdown(client, base)
		case <-ticker.C:
		}
	}
}
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	return 1
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := command(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(exitCode(err))
	}
}
