package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The preflight checks that the leased proxy can reach the public internet.
// CGV is deliberately not used here: its bot protection can return 403 to a
// plain HTTP client even when the proxy is healthy. CGV compatibility is
// exercised later by the browser-backed assignment executor.
var defaultProbeURL = "https://api.ipify.org"

// ValidateConfig verifies both the control plane (for Soxy) and the resulting
// data-plane proxy before settings become durable.
func ValidateConfig(ctx context.Context, config Config) error {
	manager, err := New(config)
	if err != nil {
		return err
	}
	probe := config.Probe
	if probe == nil {
		probe = probeProxy
	}
	proxies := append([]Proxy(nil), config.Proxies...)
	proxies = append(proxies, config.ScanProxies...)
	if manager.client != nil {
		lease, acquireErr := manager.Acquire(ctx, PurposeScan)
		if acquireErr != nil {
			return fmt.Errorf("validate Soxy lease: %w", acquireErr)
		}
		probeErr := probe(lease.Context(), *lease.Proxy())
		closeErr := lease.Close()
		if probeErr != nil || closeErr != nil {
			return fmt.Errorf("validate Soxy proxy: %w", errors.Join(probeErr, closeErr))
		}
		return nil
	}
	for _, proxy := range proxies {
		if err := probe(ctx, proxy); err != nil {
			return fmt.Errorf("validate proxy %s: %w", proxy.Server, err)
		}
	}
	return nil
}

func probeProxy(ctx context.Context, proxy Proxy) error {
	parsed, err := url.Parse(proxy.Server)
	if err != nil {
		return err
	}
	if proxy.Username != "" {
		parsed.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	transport := &http.Transport{
		Proxy:             http.ProxyURL(parsed),
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, defaultProbeURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}
