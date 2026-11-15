package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	publicIPv4Endpoint = "https://api.ipify.org"
	publicIPv6Endpoint = "https://api6.ipify.org"
	publicIPRefresh    = 30 * time.Minute
	publicIPTimeout    = 5 * time.Second
	publicIPMaxBytes   = 128
)

// These special-use ranges are not public egress addresses, even where Go's
// IsGlobalUnicast permits them. IPv6 is additionally restricted to 2000::/3.
var nonPublicIPRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

var publicIPv6Range = netip.MustParsePrefix("2000::/3")

func parsePublicIP(body string) (string, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(body))
	if err != nil || ip.Zone() != "" {
		return "", errors.New("invalid public IP response")
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !publicIPv6Range.Contains(ip)) {
		return "", errors.New("nonpublic IP response")
	}
	for _, prefix := range nonPublicIPRanges {
		if prefix.Contains(ip) {
			return "", errors.New("nonpublic IP response")
		}
	}
	return ip.String(), nil
}

func publicIPClient(network string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Report direct egress, never an environment HTTP proxy.
	dialer := &net.Dialer{Timeout: publicIPTimeout, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, _ string, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   publicIPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func lookupPublicIP(ctx context.Context, client *http.Client, endpoint, network string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, publicIPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("public IP request failed")
	}
	// Read one extra byte to reject, rather than silently truncate, large bodies.
	body, err := io.ReadAll(io.LimitReader(resp.Body, publicIPMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > publicIPMaxBytes {
		return "", errors.New("public IP response too large")
	}
	ip, err := parsePublicIP(string(body))
	if err != nil {
		return "", err
	}
	address := netip.MustParseAddr(ip)
	if (network != "tcp4" && network != "tcp6") || address.Is4() != (network == "tcp4") {
		return "", errors.New("public IP response has wrong address family")
	}
	return ip, nil
}

type publicIPCache struct {
	mu     sync.RWMutex
	ip     string
	cancel context.CancelFunc
	done   chan struct{}
}

func startPublicIP(parent context.Context, endpoint, network string) *publicIPCache {
	ctx, cancel := context.WithCancel(parent)
	c := &publicIPCache{cancel: cancel, done: make(chan struct{})}
	client := publicIPClient(network)
	go func() {
		defer close(c.done)
		defer client.CloseIdleConnections()
		c.run(ctx, func(ctx context.Context) (string, error) {
			return lookupPublicIP(ctx, client, endpoint, network)
		}, waitPublicIP)
	}()
	return c
}

func (c *publicIPCache) close() {
	c.cancel()
	<-c.done
}

func (c *publicIPCache) value() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ip
}

// run owns lookups; metrics only read the cache and never initiate network work.
func (c *publicIPCache) run(ctx context.Context, lookup func(context.Context) (string, error), wait func(context.Context, time.Duration) bool) {
	for ctx.Err() == nil {
		ip, err := lookup(ctx)
		if err != nil {
			ip = ""
		}
		c.mu.Lock()
		c.ip = ip
		c.mu.Unlock()
		if !wait(ctx, publicIPRefresh) {
			return
		}
	}
}

func waitPublicIP(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
