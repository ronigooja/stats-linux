package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
		network          string
		status           int
	}{
		{"ipv4", "8.8.8.8\n", "8.8.8.8", "tcp4", 200},
		{"ipv6", "2606:4700:4700::1111", "2606:4700:4700::1111", "tcp6", 200},
		{"ipv4-rejects-ipv6", "2606:4700:4700::1111", "", "tcp4", 200},
		{"ipv6-rejects-ipv4", "8.8.8.8", "", "tcp6", 200},
		{"ipv6-rejects-mapped-ipv4", "::ffff:8.8.8.8", "", "tcp6", 200},
		{"maximum", "8.8.8.8" + strings.Repeat(" ", publicIPMaxBytes-7), "8.8.8.8", "tcp4", 200},
		{"oversized", "8.8.8.8" + strings.Repeat(" ", publicIPMaxBytes-6), "", "tcp4", 200},
		{"invalid", "hello", "", "tcp4", 200},
		{"private", "10.0.0.1", "", "tcp4", 200},
		{"empty", "", "", "tcp4", 200},
		{"status", "8.8.8.8", "", "tcp4", 503},
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
			client := publicIPClient("tcp4") // Local server is IPv4; response family is checked separately.
			defer client.CloseIdleConnections()
			got, err := lookupPublicIP(context.Background(), client, s.URL, tc.network)
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
	client := publicIPClient("tcp4")
	defer client.CloseIdleConnections()
	if client.Timeout != 5*time.Second || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("incorrect timeout or proxy policy")
	}
	if _, err := lookupPublicIP(context.Background(), client, s.URL, "tcp4"); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect followed: count=%d, err=%v", redirected.Load(), err)
	}
	if publicIPv4Endpoint != "https://api.ipify.org" || publicIPv6Endpoint != "https://api6.ipify.org" {
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
			client := publicIPClient("tcp4")
			defer client.CloseIdleConnections()
			if mode == "timeout" {
				client.Timeout = 50 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() {
				_, err := lookupPublicIP(ctx, client, s.URL, "tcp4")
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

func TestPublicIPClientDialFamily(t *testing.T) {
	for _, tc := range []struct{ network, address, other string }{
		{"tcp4", "127.0.0.1:0", "[::1]:80"},
		{"tcp6", "[::1]:0", "127.0.0.1:80"},
	} {
		t.Run(tc.network, func(t *testing.T) {
			client := publicIPClient(tc.network)
			defer client.CloseIdleConnections()
			transport := client.Transport.(*http.Transport)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			// Even a caller requesting the opposite family cannot override the policy.
			otherNetwork := "tcp6"
			if tc.network == "tcp6" {
				otherNetwork = "tcp4"
			}
			if conn, err := transport.DialContext(ctx, otherNetwork, tc.other); err == nil {
				conn.Close()
				t.Fatal("dial accepted the opposite address family")
			}
			listener, err := net.Listen(tc.network, tc.address)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			conn, err := transport.DialContext(ctx, otherNetwork, listener.Addr().String())
			if err != nil {
				t.Fatalf("forced-family dial failed: %v", err)
			}
			conn.Close()
			accepted, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			accepted.Close()
			if client.Timeout != publicIPTimeout || transport.Proxy != nil || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
				t.Fatal("incorrect timeout, proxy, or redirect policy")
			}
		})
	}
}

func TestPublicIPCachesIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ipv4, ipv6 publicIPCache
	started, updated := make(chan struct{}), make(chan struct{})
	done4, done6 := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done4)
		ipv4.run(ctx, func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}, waitPublicIP)
	}()
	<-started
	go func() {
		defer close(done6)
		ipv6.run(ctx, func(context.Context) (string, error) {
			return "2606:4700:4700::1111", nil
		}, func(ctx context.Context, delay time.Duration) bool {
			close(updated)
			return waitPublicIP(ctx, delay)
		})
	}()
	select {
	case <-updated:
		if ipv4.value() != "" || ipv6.value() != "2606:4700:4700::1111" {
			t.Fatal("one family blocked or overwrote the other cache")
		}
	case <-time.After(time.Second):
		t.Fatal("IPv6 lookup blocked on IPv4")
	}
	cancel()
	for _, done := range []chan struct{}{done4, done6} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("worker did not exit after lifecycle cancellation")
		}
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
	if _, err := lookupPublicIP(context.Background(), client, publicIPv4Endpoint, "tcp4"); !errors.Is(err, io.ErrUnexpectedEOF) || !body.closed {
		t.Fatalf("body error=%v, closed=%v", err, body.closed)
	}
	client.Transport = publicIPRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})
	if _, err := lookupPublicIP(context.Background(), client, publicIPv4Endpoint, "tcp4"); err == nil {
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
	for _, network := range []string{"tcp4", "tcp6"} {
		worker := startPublicIP(ctx, publicIPv4Endpoint, network)
		worker.close()
	}
}

func TestPublicIPMetricsJSONCompatibility(t *testing.T) {
	for _, ips := range [][2]string{{}, {"8.8.8.8", ""}, {"", "2606:4700:4700::1111"}, {"8.8.8.8", "2606:4700:4700::1111"}} {
		encoded, err := json.Marshal(metrics{Version: 1, PublicIPv4: ips[0], PublicIPv6: ips[1]})
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &object); err != nil {
			t.Fatal(err)
		}
		if _, exists := object["public_ip"]; exists {
			t.Fatalf("unreleased single-address field remains: %s", encoded)
		}
		for i, name := range []string{"public_ipv4", "public_ipv6"} {
			field, exists := object[name]
			if exists != (ips[i] != "") {
				t.Fatalf("incorrect optional field %s: %s", name, encoded)
			}
			if exists && string(field) != `"`+ips[i]+`"` {
				t.Fatalf("not an IP string: %s", field)
			}
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
	if err := json.Unmarshal([]byte(`{"version":1,"cpu_percent":12.5}`), &decoded); err != nil || decoded.PublicIPv4 != "" || decoded.PublicIPv6 != "" || decoded.CPU != 12.5 {
		t.Fatalf("old payload decoding failed: %+v, %v", decoded, err)
	}
}
