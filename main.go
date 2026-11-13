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
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/shirou/gopsutil/v4/cpu"
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
	Version   int       `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	CPU       float64   `json:"cpu_percent"`
	Memory    capacity  `json:"memory"`
	Disk      capacity  `json:"disk"`
	Network   rates     `json:"network"`
	Load      float64   `json:"load_1m"`
	Uptime    uint64    `json:"uptime_seconds"`
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
func collect(ctx context.Context, path string, previous counters) (metrics, counters, error) {
	var m metrics
	percent, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil || len(percent) == 0 {
		return m, previous, fmt.Errorf("cpu: %v", err)
	}
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return m, previous, fmt.Errorf("memory: %w", err)
	}
	d, err := collectDisks(ctx, path)
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
	m = metrics{1, now, percent[0], capacity{vm.Used, vm.Total}, d, networkRates(previous, current), l.Load1, u}
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

func run(c config) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.caFile != "" {
		pem, err := os.ReadFile(c.caFile)
		if err != nil {
			return fmt.Errorf("read CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
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
	opts := mqtt.NewClientOptions().AddBroker(c.broker).SetProtocolVersion(4).SetClientID("stats-"+c.deviceID).SetUsername(c.username).SetPassword(c.password).SetTLSConfig(tlsConfig).SetAutoReconnect(true).SetConnectRetry(true).SetConnectRetryInterval(5*time.Second).SetWriteTimeout(5*time.Second).SetWill(base+"status", `{"online":false}`, 1, true)
	opts.SetOnConnectHandler(func(mqtt.Client) {
		// Paho runs callbacks concurrently, including during shutdown. Never
		// publish here; coalesce notifications for the sole publishing loop.
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	client := mqtt.NewClient(opts)
	token := client.Connect()
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() // Exit without DISCONNECT; a concurrent connection may have a will.
	case <-timeout.C:
		return errors.New("MQTT connection timed out")
	case <-token.Done():
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("MQTT connect: %w", err)
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	var prev counters
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
		m, next, err := collect(ctx, c.diskPath, prev)
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
func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := command(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
