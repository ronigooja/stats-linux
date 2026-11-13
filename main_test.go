package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func TestNetworkRates(t *testing.T) {
	t0 := time.Unix(100, 0)
	for _, tc := range []struct {
		name     string
		old, now counters
		want     rates
	}{
		{"normal", counters{100, 200, t0}, counters{300, 300, t0.Add(2 * time.Second)}, rates{100, 50}},
		{"first sample", counters{}, counters{300, 300, t0}, rates{}},
		{"counter reset", counters{100, 200, t0}, counters{1, 300, t0.Add(time.Second)}, rates{}},
		{"same time", counters{100, 200, t0}, counters{300, 300, t0}, rates{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := networkRates(tc.old, tc.now); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

type fakeToken struct {
	mqtt.Token
	complete bool
	err      error
}

func (t fakeToken) WaitTimeout(time.Duration) bool { return t.complete }
func (t fakeToken) Error() error                   { return t.err }

type fakePublisher struct {
	open         bool
	token        mqtt.Token
	topics       []string
	payloads     [][]byte
	disconnected bool
}

func (p *fakePublisher) IsConnectionOpen() bool { return p.open }
func (p *fakePublisher) Publish(topic string, qos byte, retained bool, value interface{}) mqtt.Token {
	if qos != 1 || !retained {
		panic("expected retained QoS 1")
	}
	p.topics = append(p.topics, topic)
	p.payloads = append(p.payloads, value.([]byte))
	return p.token
}
func (p *fakePublisher) Disconnect(uint) { p.disconnected = true }

func TestPublishDoesNotQueueWhileReconnecting(t *testing.T) {
	p := &fakePublisher{open: false}
	for i := 0; i < 100; i++ {
		if err := publish(p, "metrics", metrics{}); !errors.Is(err, errOffline) {
			t.Fatalf("expected offline error, got %v", err)
		}
	}
	if len(p.topics) != 0 {
		t.Fatalf("queued %d messages while reconnecting", len(p.topics))
	}
}

func TestAnnounceStopsOnUnresolvedOrFailedPublish(t *testing.T) {
	for _, token := range []fakeToken{{complete: false}, {complete: true, err: errors.New("write failed")}} {
		p := &fakePublisher{open: true, token: token}
		if err := announce(p, "base/", "host", "linux"); err == nil {
			t.Fatal("expected publish failure")
		}
		if len(p.topics) != 1 || p.topics[0] != "base/info" {
			t.Fatalf("added publishes after failed delivery: %v", p.topics)
		}
	}
}

func TestShutdownOnlyDisconnectsAfterOfflineAcknowledged(t *testing.T) {
	for _, tc := range []struct {
		name           string
		open           bool
		token          fakeToken
		wantDisconnect bool
	}{
		{"acknowledged", true, fakeToken{complete: true}, true},
		{"timeout", true, fakeToken{}, false},
		{"publish error", true, fakeToken{complete: true, err: errors.New("failed")}, false},
		{"reconnecting", false, fakeToken{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePublisher{open: tc.open, token: tc.token}
			err := shutdown(p, "base/")
			if p.disconnected != tc.wantDisconnect || (err == nil) != tc.wantDisconnect {
				t.Fatalf("disconnect=%v, err=%v", p.disconnected, err)
			}
			if tc.open && (len(p.payloads) != 1 || string(p.payloads[0]) != `{"online":false}`) {
				t.Fatalf("unexpected offline payloads: %s", p.payloads)
			}
		})
	}
}
func TestMetricsJSON(t *testing.T) {
	b, err := json.Marshal(metrics{Version: 1, Timestamp: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "timestamp", "cpu_percent", "memory", "disk", "network", "load_1m", "uptime_seconds"} {
		if _, ok := obj[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
}
