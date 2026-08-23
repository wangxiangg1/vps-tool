package ipcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const maxResponseSize = 128

type Probe struct {
	client *http.Client
}

func NewProbe(client *http.Client) *Probe {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &Probe{client: client}
}

func (p *Probe) IPv4(ctx context.Context) (string, error) {
	return p.fetch(ctx, "https://api.ipify.org")
}

func (p *Probe) IPv6(ctx context.Context) (string, error) {
	return p.fetch(ctx, "https://api6.ipify.org")
}

func (p *Probe) RefreshIPv4(ctx context.Context) (string, error) {
	return p.IPv4(ctx)
}

func (p *Probe) fetch(ctx context.Context, endpoint string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("create IP request: %w", err)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IP endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(body))
	parsed, err := netip.ParseAddr(value)
	if err != nil {
		return "", errors.New("IP endpoint returned an invalid address")
	}
	return parsed.String(), nil
}

type Cached struct {
	inner *Probe
	ttl   time.Duration
	mu    sync.Mutex
	ip    string
	at    time.Time
}

func NewCached(inner *Probe, ttl time.Duration) *Cached {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Cached{inner: inner, ttl: ttl}
}

func (c *Cached) IPv4(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.ip != "" && time.Since(c.at) < c.ttl {
		value := c.ip
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	value, err := c.inner.IPv4(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.ip, c.at = value, time.Now()
	c.mu.Unlock()
	return value, nil
}

func (c *Cached) RefreshIPv4(ctx context.Context) (string, error) {
	value, err := c.inner.RefreshIPv4(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.ip, c.at = value, time.Now()
	c.mu.Unlock()
	return value, nil
}

func (c *Cached) IPv6(ctx context.Context) (string, error) {
	return c.inner.IPv6(ctx)
}
