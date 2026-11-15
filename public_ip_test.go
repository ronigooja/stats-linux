package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePublicIP(t *testing.T) {
	for _, input := range []string{
		"", "not an IP", "8.8.8.8\n1.1.1.1", `{"ip":"8.8.8.8"}`, "8.8.8.8:80",
		"0.1.2.3", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.1.1", "172.16.0.1", "192.168.1.1",
		"192.0.0.1", "192.0.2.1", "192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:192.168.1.1", "64:ff9b::808:808",
		"100::1", "fc00::1", "fe80::1", "ff02::1", "2001::1", "2001:db8::1", "2002:808:808::1", "3fff::1",
		"2606:4700:4700::1111%eth0",
	} {
		t.Run(input, func(t *testing.T) {
			if ip, err := parsePublicIP(input); err == nil || ip != "" {
				t.Fatalf("accepted %q as %q", input, ip)
			}
		})
	}
	for input, want := range map[string]string{
		" 8.8.8.8\n": "8.8.8.8", "1.1.1.1": "1.1.1.1", "::ffff:8.8.8.8": "8.8.8.8",
		"2606:4700:4700:0000::1111\n": "2606:4700:4700::1111", "2001:4860:4860::8888": "2001:4860:4860::8888",
	} {
		if got, err := parsePublicIP(input); err != nil || got != want {
			t.Errorf("parse %q = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestLookupPublicIPResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"ipv4", "8.8.8.8\n", "8.8.8.8", 200},
		{"ipv6", "2606:4700:4700::1111", "2606:4700:4700::1111", 200},
		{"maximum", "8.8.8.8" + strings.Repeat(" ", publicIPMaxBytes-7), "8.8.8.8", 200},
		{"oversized", "8.8.8.8" + strings.Repeat(" ", publicIPMaxBytes-6), "", 200},
		{"invalid", "hello", "", 200},
		{"private", "10.0.0.1", "", 200},
		{"empty", "", "", 200},
		{"status", "8.8.8.8", "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("unexpected request method or credentials")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer s.Close()
			client := publicIPClient()
			defer client.CloseIdleConnections()
			got, err := lookupPublicIP(context.Background(), client, s.URL)
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestPublicIPClientPolicy(t *testing.T) {
	var redirected atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			redirected.Add(1)
			io.WriteString(w, "8.8.8.8")
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer s.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := publicIPClient()
	defer client.CloseIdleConnections()
	if client.Timeout != 5*time.Second || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("incorrect timeout or proxy policy")
	}
	if _, err := lookupPublicIP(context.Background(), client, s.URL); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect followed: count=%d, err=%v", redirected.Load(), err)
	}
	if publicIPEndpoint != "https://api64.ipify.org" {
		t.Fatal("unexpected endpoint")
	}
}

func TestLookupPublicIPTimeoutAndCancel(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := publicIPClient()
			defer client.CloseIdleConnections()
			if mode == "timeout" {
				client.Timeout = 50 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() {
				_, err := lookupPublicIP(ctx, client, s.URL)
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil || (mode == "cancel" && !errors.Is(err, context.Canceled)) || (mode == "timeout" && !errors.Is(err, context.DeadlineExceeded)) {
					t.Fatalf("unexpected error: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lookup did not stop")
			}
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatal("server request was not canceled")
			}
		})
	}
}

type publicIPRoundTrip func(*http.Request) (*http.Response, error)

func (f publicIPRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type publicIPBadBody struct{ closed bool }

func (b *publicIPBadBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *publicIPBadBody) Close() error             { b.closed = true; return nil }

func TestLookupPublicIPErrors(t *testing.T) {
	body := &publicIPBadBody{}
	client := &http.Client{Transport: publicIPRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > publicIPTimeout {
			t.Error("request is missing its bounded context deadline")
		}
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	if _, err := lookupPublicIP(context.Background(), client, publicIPEndpoint); !errors.Is(err, io.ErrUnexpectedEOF) || !body.closed {
		t.Fatalf("body error=%v, closed=%v", err, body.closed)
	}
	client.Transport = publicIPRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})
	if _, err := lookupPublicIP(context.Background(), client, publicIPEndpoint); err == nil {
		t.Fatal("network error ignored")
	}
}

func TestPublicIPCacheSchedule(t *testing.T) {
	c := &publicIPCache{}
	calls := 0
	waits := 0
	wantIPs := []string{"", "8.8.8.8", "8.8.8.8", "1.1.1.1"}
	wantDelays := []time.Duration{5 * time.Minute, 30 * time.Minute, 5 * time.Minute, 30 * time.Minute}
	c.run(context.Background(), func(context.Context) (string, error) {
		calls++
		switch calls {
		case 2:
			return "8.8.8.8", nil
		case 4:
			return "1.1.1.1", nil
		default:
			return "", errors.New("temporary failure")
		}
	}, func(_ context.Context, delay time.Duration) bool {
		if calls != waits+1 || delay != wantDelays[waits] {
			t.Fatalf("calls=%d waits=%d delay=%v", calls, waits, delay)
		}
		for i := 0; i < 100; i++ {
			if got := c.value(); got != wantIPs[waits] {
				t.Fatalf("cache=%q, want %q", got, wantIPs[waits])
			}
		}
		waits++
		return waits < len(wantDelays)
	})
	if calls != 4 {
		t.Fatalf("unexpected lookup count: %d", calls)
	}
}

func TestPublicIPCacheDoesNotBlockReaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &publicIPCache{}
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		c.run(ctx, func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}, waitPublicIP)
	}()
	<-started
	read := make(chan string, 1)
	go func() { read <- c.value() }()
	select {
	case ip := <-read:
		if ip != "" {
			t.Fatalf("unknown cache = %q", ip)
		}
	case <-time.After(time.Second):
		t.Fatal("cache read blocked on lookup")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit")
	}
	if waitPublicIP(ctx, publicIPRefresh) {
		t.Fatal("canceled wait continued")
	}
	c.run(ctx, func(context.Context) (string, error) {
		t.Fatal("lookup started after cancellation")
		return "", nil
	}, waitPublicIP)
	// Starting and closing with an already-canceled lifecycle makes no request.
	worker := startPublicIP(ctx)
	worker.close()
}

func TestPublicIPMetricsJSONCompatibility(t *testing.T) {
	for _, ip := range []string{"", "8.8.8.8", "2606:4700:4700::1111"} {
		encoded, err := json.Marshal(metrics{Version: 1, PublicIP: ip})
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		field, exists := object["public_ip"]
		if exists != (ip != "") {
			t.Fatalf("incorrect optional field: %s", encoded)
		}
		if exists && string(field) != `"`+ip+`"` {
			t.Fatalf("not an IP string: %s", field)
		}
		// A legacy consumer ignores the additive field without a schema bump.
		var legacy struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(encoded, &legacy); err != nil || legacy.Version != 1 {
			t.Fatalf("legacy decoding failed: %+v, %v", legacy, err)
		}
	}
	var decoded metrics
	if err := json.Unmarshal([]byte(`{"version":1,"cpu_percent":12.5}`), &decoded); err != nil || decoded.PublicIP != "" || decoded.CPU != 12.5 {
		t.Fatalf("old payload decoding failed: %+v, %v", decoded, err)
	}
}
