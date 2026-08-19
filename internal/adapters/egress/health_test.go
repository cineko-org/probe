package egress

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidateConfigStandardProxies(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	config := Config{
		Proxies: []Proxy{{Server: "http://one:1"}, {Server: "socks5://two:2"}},
		Probe: func(_ context.Context, proxy Proxy) error {
			calls.Add(1)
			if proxy.Server == "socks5://two:2" {
				return errors.New("offline")
			}
			return nil
		},
	}
	if err := ValidateConfig(context.Background(), config); err == nil || !strings.Contains(err.Error(), "socks5://two:2") {
		t.Fatalf("ValidateConfig() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("probe calls = %d", calls.Load())
	}
	config.Probe = func(context.Context, Proxy) error { return nil }
	if err := ValidateConfig(context.Background(), config); err != nil {
		t.Fatalf("ValidateConfig(success) error = %v", err)
	}
	if err := ValidateConfig(context.Background(), Config{Proxies: []Proxy{{Server: "ftp://bad:1"}}}); err == nil {
		t.Fatal("ValidateConfig(invalid) error = nil")
	}
}

func TestValidateConfigManagedSoxy(t *testing.T) {
	t.Parallel()
	var releaseStatus atomic.Int32
	releaseStatus.Store(http.StatusNoContent)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/slots":
			writeTestJSON(writer, http.StatusOK, map[string]any{"slots": []map[string]any{{"id": "slot", "status": "available", "current_ip": "192.0.2.1"}}})
		case "POST /v1/sessions":
			writeTestJSON(writer, http.StatusCreated, readySession("health"))
		case "DELETE /v1/sessions/health":
			writer.WriteHeader(int(releaseStatus.Load()))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	config := Config{
		SoxyURL: server.URL, SoxyToken: "token", Random: bytes.NewReader(make([]byte, 32)),
		Probe: func(_ context.Context, proxy Proxy) error {
			if proxy.Server != "http://127.0.0.1:11001" {
				t.Fatalf("proxy = %+v", proxy)
			}
			return nil
		},
	}
	if err := ValidateConfig(context.Background(), config); err != nil {
		t.Fatalf("ValidateConfig() error = %v", err)
	}
	releaseStatus.Store(http.StatusInternalServerError)
	if err := ValidateConfig(context.Background(), config); err == nil || !strings.Contains(err.Error(), "validate Soxy proxy") {
		t.Fatalf("ValidateConfig(release failure) error = %v", err)
	}
	config.Probe = func(context.Context, Proxy) error { return errors.New("blocked") }
	if err := ValidateConfig(context.Background(), config); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("ValidateConfig(probe failure) error = %v", err)
	}
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("control offline")
	})}
	if err := ValidateConfig(context.Background(), config); err == nil || !strings.Contains(err.Error(), "validate Soxy lease") {
		t.Fatalf("ValidateConfig(acquire failure) error = %v", err)
	}
}

func TestProbeProxy(t *testing.T) {
	oldURL := defaultProbeURL
	t.Cleanup(func() { defaultProbeURL = oldURL })
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Proxy-Authorization") == "" {
			http.Error(writer, "auth required", http.StatusProxyAuthRequired)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()
	defaultProbeURL = "http://health.example.test/status"
	parsed, err := ParseProxy(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Username, parsed.Password = "user", "password"
	if err := probeProxy(context.Background(), parsed); err != nil {
		t.Fatalf("probeProxy() error = %v", err)
	}
	if err := ValidateConfig(context.Background(), Config{Proxies: []Proxy{parsed}}); err != nil {
		t.Fatalf("ValidateConfig(default probe) error = %v", err)
	}
	parsed.Username = ""
	if err := probeProxy(context.Background(), parsed); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("probeProxy(status) error = %v", err)
	}
	defaultProbeURL = "://invalid"
	if err := probeProxy(context.Background(), parsed); err == nil {
		t.Fatal("probeProxy(invalid request) error = nil")
	}
	if err := probeProxy(context.Background(), Proxy{Server: "://invalid"}); err == nil {
		t.Fatal("probeProxy(invalid proxy) error = nil")
	}
	defaultProbeURL = "http://health.example.test/status"
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeProxy(cancelled, parsed); !errors.Is(err, context.Canceled) {
		t.Fatalf("probeProxy(cancelled) error = %v", err)
	}
}

func TestDefaultProbeURLIsProviderNeutral(t *testing.T) {
	if defaultProbeURL != "https://api.ipify.org" {
		t.Fatalf("defaultProbeURL = %q", defaultProbeURL)
	}
}
