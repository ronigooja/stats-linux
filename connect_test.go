package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

func TestMQTTRefusalMessage(t *testing.T) {
	for _, tc := range []struct {
		code byte
		want string
	}{
		{1, "MQTT broker rejected the protocol version (MQTT 3.1.1 required)"},
		{2, "MQTT broker rejected the client identifier (check STATS_DEVICE_ID)"},
		{3, "MQTT broker is unavailable"},
		{4, "MQTT broker rejected the username or password (check STATS_USERNAME / STATS_PASSWORD)"},
		{5, "MQTT broker rejected the client (not authorized)"},
		{255, "MQTT broker rejected the connection (protocol violation)"},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			for _, err := range []error{packets.ConnErrors[tc.code], fmt.Errorf("secret-url: %w", packets.ConnErrors[tc.code])} {
				if got := mqttRefusalMessage(err); got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
	for _, err := range []error{nil, packets.ErrorNetworkError, errors.New("ssl://user:secret@host")} {
		if got := mqttRefusalMessage(err); got != "" {
			t.Fatalf("unexpected message %q", got)
		}
	}
}

// Use the pinned Paho handshake against an in-memory broker, not an external daemon.
func TestInitialConnectionRefusal(t *testing.T) {
	for _, code := range []byte{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			opts := mqtt.NewClientOptions().AddBroker("tcp://test:1883").SetProtocolVersion(4).SetConnectRetry(false)
			opts.CustomOpenConnectionFn = func(*url.URL, mqtt.ClientOptions) (net.Conn, error) {
				client, broker := net.Pipe()
				go func() {
					defer broker.Close()
					if _, err := packets.ReadPacket(broker); err != nil {
						return
					}
					ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
					ack.ReturnCode = code
					_ = ack.Write(broker)
				}()
				return client, nil
			}
			client := mqtt.NewClient(opts)
			err := connectInitial(context.Background(), client.Connect, time.Second, time.Millisecond)
			if err == nil || err.Error() != mqttRefusalMessage(packets.ConnErrors[code]) {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

type connectToken struct {
	mqtt.Token
	done chan struct{}
	err  error
}

func (t connectToken) Done() <-chan struct{} { return t.done }
func (t connectToken) Error() error          { return t.err }

func TestInitialConnectionWait(t *testing.T) {
	done := make(chan struct{})
	close(done)
	for _, tc := range []struct {
		name     string
		err      error
		complete bool
		want     string
	}{
		{"success", nil, true, ""},
		{"pending timeout", nil, false, "MQTT connection timed out"},
		{"network retries", errors.New("ssl://user:secret@host"), true, "MQTT connection timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := connectInitial(context.Background(), func() mqtt.Token {
				calls++
				ch := make(chan struct{})
				if tc.complete {
					ch = done
				}
				return connectToken{done: ch, err: tc.err}
			}, 20*time.Millisecond, time.Millisecond)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unexpected result: %v", err)
			}
			if tc.err != nil && calls < 2 {
				t.Fatal("network failure was not retried")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connectInitial(ctx, func() mqtt.Token { return connectToken{done: make(chan struct{})} }, time.Second, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
