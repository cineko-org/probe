package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

type secretFileStub struct {
	io.Reader
	info    os.FileInfo
	statErr error
}

func (file *secretFileStub) Stat() (os.FileInfo, error) { return file.info, file.statErr }
func (file *secretFileStub) Close() error               { return nil }

type secretFileOpenStub struct {
	file secretFile
	err  error
}

func (stub secretFileOpenStub) open(string) (secretFile, error) {
	return stub.file, stub.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestNewFromEnvironment(t *testing.T) {
	t.Setenv("CINEKO_SOXY_URL", "")
	t.Setenv("CINEKO_SOXY_API_TOKEN", "")
	t.Setenv("CINEKO_SOXY_API_TOKEN_FILE", "")
	t.Setenv("CINEKO_SOXY_SESSION_TTL", "")
	t.Setenv("CINEKO_SCAN_PROXIES", "")
	t.Setenv("CINEKO_SCAN_PROXIES_FILE", "")
	if _, err := NewFromEnvironment(); err != nil {
		t.Fatalf("NewFromEnvironment() error = %v", err)
	}
	config, err := ConfigFromEnvironment()
	if err != nil || config.SoxyURL != "" || config.SoxyToken != "" {
		t.Fatalf("ConfigFromEnvironment() = %+v, %v", config, err)
	}
	t.Setenv("CINEKO_SOXY_URL", "https://soxy.example.test")
	if _, err := NewFromEnvironment(); err == nil {
		t.Fatal("NewFromEnvironment(URL only) error = nil")
	}
	t.Setenv("CINEKO_SOXY_URL", "")
	t.Setenv("CINEKO_SOXY_SESSION_TTL", "invalid")
	if _, err := NewFromEnvironment(); err == nil {
		t.Fatal("NewFromEnvironment() error = nil")
	}
}

func TestConfigFromLookup(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"CINEKO_SOXY_URL":            " https://soxy.example.test ",
		"CINEKO_SOXY_API_TOKEN_FILE": "/run/secrets/soxy-token",
		"CINEKO_SOXY_SESSION_TTL":    "45m",
	}
	config, err := configFromSources(func(key string) (string, bool) {
		value, exists := values[key]
		return value, exists
	}, func(path string, limit int64) (string, error) {
		if path != "/run/secrets/soxy-token" || limit != maximumTokenBytes {
			t.Fatalf("secret read = %q, %d", path, limit)
		}
		return " secret\n", nil
	})
	if err != nil {
		t.Fatalf("configFromLookup() error = %v", err)
	}
	if config.SoxyURL != "https://soxy.example.test" || config.SoxyToken != "secret" || config.SessionTTL != 45*time.Minute {
		t.Fatalf("configFromLookup() = %+v", config)
	}
	if len(config.ScanProxies) != 0 {
		t.Fatalf("unexpected scan proxies = %+v", config.ScanProxies)
	}
}

func TestConfigFromSecretFiles(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"CINEKO_SCAN_PROXIES_FILE": "/run/secrets/scan-proxies",
	}
	config, err := configFromSources(func(key string) (string, bool) {
		value, exists := values[key]
		return value, exists
	}, func(path string, limit int64) (string, error) {
		if path != "/run/secrets/scan-proxies" || limit != maximumProxyBytes {
			t.Fatalf("secret read = %q, %d", path, limit)
		}
		return "http://alpha:one@127.0.0.1:11001\nsocks5://127.0.0.1:11002/", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Proxy{
		{Server: "http://127.0.0.1:11001", Username: "alpha", Password: "one"},
		{Server: "socks5://127.0.0.1:11002"},
	}
	if !reflect.DeepEqual(config.ScanProxies, want) {
		t.Fatalf("scan proxies = %+v, want %+v", config.ScanProxies, want)
	}
}

func TestConfigFromLookupRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "TTL", key: "CINEKO_SOXY_SESSION_TTL", value: "eventually"},
		{name: "plaintext token", key: "CINEKO_SOXY_API_TOKEN", value: "secret"},
		{name: "plaintext proxy", key: "CINEKO_SCAN_PROXIES", value: "http://127.0.0.1:1"},
		{name: "relative token file", key: "CINEKO_SOXY_API_TOKEN_FILE", value: "secret/token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configFromLookup(func(key string) (string, bool) {
				if key == test.key {
					return test.value, true
				}
				return "", false
			})
			if err == nil {
				t.Fatal("configFromLookup() error = nil")
			}
		})
	}
}

func TestConfigFromSourcesRedactsSecretsAndReadFailures(t *testing.T) {
	t.Parallel()
	const redactionSentinel = "credential-must-not-leak"
	tests := []struct {
		name   string
		values map[string]string
		read   func(string, int64) (string, error)
	}{
		{
			name: "token read", values: map[string]string{"CINEKO_SOXY_API_TOKEN_FILE": "/run/secrets/token"},
			read: func(string, int64) (string, error) { return "", errors.New(redactionSentinel) },
		},
		{
			name: "proxy read", values: map[string]string{"CINEKO_SCAN_PROXIES_FILE": "/run/secrets/proxies"},
			read: func(string, int64) (string, error) { return "", errors.New(redactionSentinel) },
		},
		{
			name: "proxy parse", values: map[string]string{"CINEKO_SCAN_PROXIES_FILE": "/run/secrets/proxies"},
			read: func(string, int64) (string, error) { return "http://user:" + redactionSentinel + "@missing-port", nil },
		},
		{
			name: "empty token", values: map[string]string{"CINEKO_SOXY_API_TOKEN_FILE": "/run/secrets/token"},
			read: func(string, int64) (string, error) { return " \n", nil },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := configFromSources(func(key string) (string, bool) {
				value, exists := test.values[key]
				return value, exists
			}, test.read)
			if err == nil || strings.Contains(err.Error(), redactionSentinel) {
				t.Fatalf("redacted error = %v", err)
			}
		})
	}
}

func TestReadSecretFileDefensiveIO(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Lstat(replacement)
	if err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func(string) (secretFile, error){
		"open": (secretFileOpenStub{err: errors.New("open")}).open,
		"stat": (secretFileOpenStub{file: &secretFileStub{
			Reader: strings.NewReader("secret"), statErr: errors.New("stat"),
		}}).open,
		"changed": (secretFileOpenStub{file: &secretFileStub{
			Reader: strings.NewReader("secret"), info: replacementInfo,
		}}).open,
		"read": (secretFileOpenStub{file: &secretFileStub{
			Reader: errorReader{err: io.ErrUnexpectedEOF}, info: info,
		}}).open,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readSecretFileWith(path, 16, os.Lstat, open); err == nil {
				t.Fatalf("%s failure was accepted", name)
			}
		})
	}
	if _, err := readSecretFileWith(path, 16, func(string) (os.FileInfo, error) {
		return nil, errors.New("lstat")
	}, nil); err == nil {
		t.Fatal("lstat failure was accepted")
	}
}

func TestReadSecretFile(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o400, 0o440, 0o600, 0o640} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			value, err := readSecretFile(path, 16)
			if err != nil || value != "secret\n" {
				t.Fatalf("readSecretFile() = %q, %v", value, err)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path, 3); err == nil {
		t.Fatal("oversized secret was accepted")
	}
	if _, err := readSecretFile(filepath.Join(t.TempDir(), "missing"), 16); err == nil {
		t.Fatal("missing secret was accepted")
	}
	directory := t.TempDir()
	if _, err := readSecretFile(directory, 16); err == nil {
		t.Fatal("directory secret was accepted")
	}
	for _, mode := range []os.FileMode{0o644, 0o660} {
		t.Run("reject-"+mode.String(), func(t *testing.T) {
			wide := filepath.Join(t.TempDir(), "wide-secret")
			if err := os.WriteFile(wide, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(wide, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := readSecretFile(wide, 16); err == nil {
				t.Fatalf("secret mode %04o was accepted", mode)
			}
		})
	}
	symlink := filepath.Join(t.TempDir(), "secret-link")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(symlink, 16); err == nil {
		t.Fatal("symlink secret was accepted")
	}
}

func TestParseProxy(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"", "%", "ftp://proxy.example:21", "http://proxy.example", "http://proxy.example:0",
		"http://proxy.example:65536", "http://proxy.example:8080/path", "http://proxy.example:8080?q=1",
		"http://proxy.example:8080#fragment",
	} {
		if _, err := ParseProxy(rawURL); err == nil {
			t.Errorf("ParseProxy(%q) error = nil", rawURL)
		}
	}
	proxy, err := ParseProxy("https://user:p%40ss@[2001:db8::1]:8443/")
	if err != nil {
		t.Fatalf("ParseProxy() error = %v", err)
	}
	if proxy.Server != "https://[2001:db8::1]:8443" || proxy.Username != "user" || proxy.Password != "p@ss" {
		t.Fatalf("ParseProxy() = %+v", proxy)
	}
}

func TestManagerDirectAndStaticPolicies(t *testing.T) {
	t.Parallel()
	direct, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	lease, err := direct.Acquire(context.Background(), PurposeSession)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if lease.Proxy() != nil {
		t.Fatalf("direct proxy = %+v", lease.Proxy())
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !errors.Is(lease.Context().Err(), context.Canceled) {
		t.Fatalf("lease context error = %v", lease.Context().Err())
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	static, err := New(Config{
		ScanProxies: []Proxy{{Server: "http://one:1"}, {Server: "http://two:2"}},
		Random:      bytes.NewReader([]byte{1}),
	})
	if err != nil {
		t.Fatalf("New(static) error = %v", err)
	}
	staticLease, err := static.Acquire(context.Background(), PurposeScan)
	if err != nil {
		t.Fatalf("Acquire(scan) error = %v", err)
	}
	if got := staticLease.Proxy(); got == nil || got.Server != "http://two:2" {
		t.Fatalf("static scan proxy = %+v", got)
	}
	_ = staticLease.Close()
	static.random = errorReader{err: io.ErrUnexpectedEOF}
	if _, err := static.Acquire(context.Background(), PurposeScan); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Acquire(scan random failure) error = %v", err)
	}
}

func TestAssignmentPolicyFallsBackToDirectAndSelectsConfiguredProxy(t *testing.T) {
	t.Parallel()
	direct, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	directLease, err := direct.AcquireForPolicy(
		context.Background(), PurposeScan, PolicyScanDefault,
	)
	if err != nil {
		t.Fatalf("scan_default without proxy error = %v", err)
	}
	if directLease.Proxy() != nil {
		t.Fatalf("scan_default direct proxy = %+v", directLease.Proxy())
	}
	_ = directLease.Close()
	for _, policyID := range []string{"", "direct", "unknown"} {
		if _, err := direct.AcquireForPolicy(
			context.Background(), PurposeScan, policyID,
		); !errors.Is(err, ErrUnknownPolicy) {
			t.Errorf("policy %q error = %v", policyID, err)
		}
	}
	if _, err := direct.AcquireForPolicy(
		context.Background(), PurposeSession, PolicyScanDefault,
	); !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("session assignment policy error = %v", err)
	}

	proxied, err := New(Config{
		ScanProxies: []Proxy{{Server: "http://one:1"}, {Server: "http://two:2"}},
		Random:      bytes.NewReader([]byte{1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := proxied.AcquireForPolicy(context.Background(), PurposeScan, PolicyScanDefault)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	if proxy := lease.Proxy(); proxy == nil || proxy.Server != "http://two:2" {
		t.Fatalf("selected policy proxy = %+v", proxy)
	}
}

func TestManagerStandardProxiesApplyToSessionsAndScans(t *testing.T) {
	t.Parallel()
	manager, err := New(Config{
		Proxies: []Proxy{
			{Server: "http://embedded:secret@one:1"}, // #nosec G101 -- synthetic credential verifies proxy parsing.
			{Server: "socks5://two:2", Username: "override", Password: "password"},
		},
		Random: bytes.NewReader([]byte{0, 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(context.Background(), PurposeSession)
	if err != nil || session.Proxy() == nil || session.Proxy().Username != "embedded" {
		t.Fatalf("session = %+v, error = %v", session.Proxy(), err)
	}
	_ = session.Close()
	scan, err := manager.Acquire(context.Background(), PurposeScan)
	if err != nil || scan.Proxy() == nil || scan.Proxy().Server != "socks5://two:2" || scan.Proxy().Username != "override" {
		t.Fatalf("scan = %+v, error = %v", scan.Proxy(), err)
	}
	_ = scan.Close()
	manager.random = errorReader{err: io.ErrUnexpectedEOF}
	if _, err := manager.Acquire(context.Background(), PurposeSession); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Acquire(session random failure) error = %v", err)
	}
}

func TestManagerRejectsInvalidConfigurationAndPurpose(t *testing.T) {
	t.Parallel()
	for _, config := range []Config{
		{SessionTTL: time.Minute},
		{SessionTTL: 25 * time.Hour},
		{MaxRenewFailures: -1},
		{RenewInterval: -1},
		{SoxyURL: "https://soxy.test"},
		{SoxyToken: "token"},
		{SoxyURL: "://bad", SoxyToken: "token"},
		{SoxyURL: "https://soxy.test", SoxyToken: "token", Proxies: []Proxy{{Server: "http://one:1"}}},
		{Proxies: []Proxy{{Server: "ftp://bad:1"}}},
		{ScanProxies: []Proxy{{Server: "ftp://bad:1"}}},
	} {
		if _, err := New(config); err == nil {
			t.Errorf("New(%+v) error = nil", config)
		}
	}
	manager, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(nil, PurposeSession); err == nil { //nolint:staticcheck // verifies the nil boundary
		t.Fatal("Acquire(nil) error = nil")
	}
	if _, err := manager.Acquire(context.Background(), Purpose("other")); err == nil {
		t.Fatal("Acquire(other) error = nil")
	}
}

func TestManagedScanChoosesTargetSlotAndReleases(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var createdSlots []string
	var releases int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/slots":
			writeTestJSON(writer, http.StatusOK, map[string]any{"slots": []map[string]any{
				{"id": "slot-01", "status": "available", "current_ip": "192.0.2.1"},
				{"id": "slot-busy", "status": "leased", "current_ip": "192.0.2.2"},
				{"id": "slot-03", "status": "available", "current_ip": "192.0.2.3"},
			}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions":
			var payload struct {
				TTLSeconds int64  `json:"ttl_seconds"`
				SlotID     string `json:"slot_id"`
			}
			_ = json.NewDecoder(request.Body).Decode(&payload)
			mu.Lock()
			createdSlots = append(createdSlots, payload.SlotID)
			mu.Unlock()
			if payload.TTLSeconds != 300 {
				t.Errorf("ttl_seconds = %d", payload.TTLSeconds)
			}
			if payload.SlotID == "slot-03" {
				writeTestJSON(writer, http.StatusConflict, map[string]any{"error": map[string]string{"code": "no_capacity"}})
				return
			}
			writeTestJSON(writer, http.StatusCreated, readySession("scan-session"))
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/sessions/scan-session":
			mu.Lock()
			releases++
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager, err := New(Config{
		SoxyURL: server.URL, SoxyToken: "test-token", SessionTTL: 5 * time.Minute,
		Random: bytes.NewReader([]byte{1, 0}), RenewInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	lease, err := manager.AcquireForPolicy(context.Background(), PurposeScan, PolicyScanDefault)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if got := lease.Proxy(); got == nil || got.Server != "http://127.0.0.1:11001" {
		t.Fatalf("managed proxy = %+v", got)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(createdSlots, []string{"slot-03", "slot-01"}) || releases != 1 {
		t.Fatalf("created slots = %v, releases = %d", createdSlots, releases)
	}
}

func TestManagedLeaseRenewalFailureCancelsTask(t *testing.T) {
	t.Parallel()
	var extends atomic.Int64
	var releases atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions":
			writeTestJSON(writer, http.StatusCreated, readySession("renew-session"))
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions/renew-session/extend":
			extends.Add(1)
			writeTestJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "unavailable"}})
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/sessions/renew-session":
			releases.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager, err := New(Config{
		SoxyURL: server.URL, SoxyToken: "token", SessionTTL: 5 * time.Minute,
		RenewInterval: 10 * time.Millisecond, MaxRenewFailures: 2,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	lease, err := manager.Acquire(context.Background(), PurposeSession)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("lease context was not cancelled")
	}
	if !errors.Is(context.Cause(lease.Context()), ErrLeaseRenewal) {
		t.Fatalf("lease cause = %v", context.Cause(lease.Context()))
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if extends.Load() != 2 || releases.Load() != 1 {
		t.Fatalf("extends = %d, releases = %d", extends.Load(), releases.Load())
	}
}

func TestManagedSessionValidationAndCapacity(t *testing.T) {
	t.Parallel()
	t.Run("no slots", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeTestJSON(writer, http.StatusOK, map[string]any{"slots": []any{}})
		}))
		defer server.Close()
		manager, err := New(Config{SoxyURL: server.URL, SoxyToken: "token"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = manager.Acquire(context.Background(), PurposeScan)
		if !errors.Is(err, ErrNoProxyCapacity) {
			t.Fatalf("Acquire() error = %v", err)
		}
	})
	t.Run("selection failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeTestJSON(writer, http.StatusOK, map[string]any{"slots": []map[string]any{
				{"id": "slot-01", "status": "available", "current_ip": "192.0.2.1"},
				{"id": "slot-02", "status": "available", "current_ip": "192.0.2.2"},
			}})
		}))
		defer server.Close()
		manager, err := New(Config{SoxyURL: server.URL, SoxyToken: "token", Random: errorReader{err: io.ErrClosedPipe}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = manager.Acquire(context.Background(), PurposeScan)
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Acquire() error = %v", err)
		}
	})
	t.Run("session error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/v1/slots" {
				writeTestJSON(writer, http.StatusOK, map[string]any{"slots": []map[string]any{
					{"id": "slot-01", "status": "available", "current_ip": "192.0.2.1"},
				}})
				return
			}
			writeTestJSON(writer, http.StatusInternalServerError, map[string]string{"status": "failed"})
		}))
		defer server.Close()
		manager, err := New(Config{SoxyURL: server.URL, SoxyToken: "token", Random: bytes.NewReader([]byte{0})})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Acquire(context.Background(), PurposeScan); err == nil {
			t.Fatal("Acquire() error = nil")
		}
	})
	t.Run("raced slots exhaust capacity", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/v1/slots" {
				writeTestJSON(writer, http.StatusOK, map[string]any{"slots": []map[string]any{
					{"id": "slot-01", "status": "available", "current_ip": "192.0.2.1"},
				}})
				return
			}
			writeTestJSON(writer, http.StatusConflict, map[string]any{"error": map[string]string{"code": "no_capacity"}})
		}))
		defer server.Close()
		manager, err := New(Config{SoxyURL: server.URL, SoxyToken: "token", Random: bytes.NewReader([]byte{0})})
		if err != nil {
			t.Fatal(err)
		}
		_, err = manager.Acquire(context.Background(), PurposeScan)
		if !errors.Is(err, ErrNoProxyCapacity) {
			t.Fatalf("Acquire() error = %v", err)
		}
	})
	t.Run("invalid proxy is released", func(t *testing.T) {
		var releases atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodDelete {
				releases.Add(1)
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			writeTestJSON(writer, http.StatusCreated, map[string]any{
				"id": "invalid-session", "status": "active", "ready": true,
				"proxy": map[string]any{"scheme": "http", "host": "", "port": 0},
			})
		}))
		defer server.Close()
		manager, err := New(Config{SoxyURL: server.URL, SoxyToken: "token"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Acquire(context.Background(), PurposeSession); err == nil {
			t.Fatal("Acquire() error = nil")
		}
		if releases.Load() != 1 {
			t.Fatalf("releases = %d", releases.Load())
		}
	})
}

func TestProxyHelpers(t *testing.T) {
	t.Parallel()
	if proxyUser("", "secret") != nil {
		t.Fatal("empty username produced user info")
	}
	if user := proxyUser("user", ""); user == nil || user.String() != "user" {
		t.Fatalf("username-only user info = %v", user)
	}
	if user := proxyUser("user", "secret"); user == nil || user.String() != "user:secret" {
		t.Fatalf("credential user info = %v", user)
	}
	if _, err := randomIndex(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("randomIndex(empty) error = nil")
	}
	if _, err := randomIndex(errorReader{err: io.ErrClosedPipe}, 2); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("randomIndex(error) = %v", err)
	}
}

func TestManagedLeaseRenewsAndStops(t *testing.T) {
	t.Parallel()
	renewed := make(chan struct{}, 1)
	var renewalCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions":
			writeTestJSON(writer, http.StatusCreated, map[string]any{
				"id": "success-session", "status": "active", "ready": true,
				"proxy": map[string]any{
					"scheme": "http", "host": "2001:db8::1", "port": 11001,
					"username": "user", "password": "secret",
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions/success-session/extend":
			if renewalCount.Add(1) >= 2 {
				select {
				case renewed <- struct{}{}:
				default:
				}
			}
			writeTestJSON(writer, http.StatusOK, readySession("success-session"))
		case request.Method == http.MethodDelete:
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager, err := New(Config{
		SoxyURL: server.URL, SoxyToken: "token", SessionTTL: 5 * time.Minute, RenewInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), PurposeSession)
	if err != nil {
		t.Fatal(err)
	}
	if proxy := lease.Proxy(); proxy == nil || proxy.Server != "http://[2001:db8::1]:11001" || proxy.Username != "user" {
		t.Fatalf("proxy = %+v", proxy)
	}
	select {
	case <-renewed:
	case <-time.After(time.Second):
		t.Fatal("lease was not renewed")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if got := renewalRetryDelay(time.Millisecond); got != time.Millisecond {
		t.Fatalf("short retry delay = %s", got)
	}
	if got := renewalRetryDelay(time.Hour); got != 5*time.Second {
		t.Fatalf("long retry delay = %s", got)
	}
}

func TestSoxyClientRejectsInvalidOriginAndMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"", "ftp://soxy.test", "http://soxy.test", "http://192.0.2.1:8080", "https://user@soxy.test",
		"https://soxy.test/path", "https://soxy.test?q=1", "http://172.15.255.255:8080", "http://172.32.0.1:8080",
	} {
		if _, err := newSoxyClient(rawURL, "token", nil); err == nil {
			t.Errorf("newSoxyClient(%q) error = nil", rawURL)
		}
	}
	for _, rawURL := range []string{"http://localhost:8080", "http://api.localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := newSoxyClient(rawURL, "token", nil); err != nil {
			t.Errorf("newSoxyClient(%q) loopback error = %v", rawURL, err)
		}
	}
	for _, rawURL := range []string{
		"http://10.0.0.10:8080", "http://172.16.0.10:8080", "http://172.31.255.254:8080",
		"http://192.168.50.10:8080", "http://[fd00::10]:8080",
	} {
		if _, err := newSoxyClient(rawURL, "token", nil); err != nil {
			t.Errorf("newSoxyClient(%q) private IP origin error = %v", rawURL, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "not-json")
	}))
	defer server.Close()
	client, err := newSoxyClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.availableSlots(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("availableSlots() error = %v", err)
	}
}

func TestSoxyClientDoesNotForwardBearerAcrossRedirects(t *testing.T) {
	t.Parallel()
	var redirectedRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirectedRequests.Add(1)
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("redirected authorization = %q", authorization)
		}
		writer.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client, err := newSoxyClient(origin.URL, "sensitive-token", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.createSession(context.Background(), 5*time.Minute, ""); err == nil {
		t.Fatal("redirecting Soxy response was accepted")
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("redirect target requests = %d", redirectedRequests.Load())
	}
}

func TestSoxyClientErrorAndDefensivePaths(t *testing.T) {
	t.Parallel()
	if got := (&soxyAPIError{Status: 500}).Error(); got != "Soxy returned HTTP 500" {
		t.Fatalf("Error() = %q", got)
	}
	var releaseCalls atomic.Int64
	var invalidReleaseCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/sessions":
			writeTestJSON(writer, http.StatusCreated, map[string]any{"id": "not-ready", "status": "active", "ready": false})
		case "/v1/sessions/not-ready":
			invalidReleaseCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		case "/v1/sessions/missing":
			releaseCalls.Add(1)
			writeTestJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := newSoxyClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.createSession(context.Background(), 5*time.Minute, ""); err == nil {
		t.Fatal("createSession(not ready) error = nil")
	}
	if invalidReleaseCalls.Load() != 1 {
		t.Fatalf("invalid session releases = %d", invalidReleaseCalls.Load())
	}
	if err := client.releaseSession(context.Background(), "missing"); err != nil || releaseCalls.Load() != 1 {
		t.Fatalf("releaseSession(missing) = %v, calls = %d", err, releaseCalls.Load())
	}
	if err := client.request(context.Background(), http.MethodPost, "/", make(chan int), http.StatusOK, nil); err == nil {
		t.Fatal("request(unencodable) error = nil")
	}
	if err := client.request(context.Background(), http.MethodGet, "\n", nil, http.StatusOK, nil); err == nil {
		t.Fatal("request(invalid URL) error = nil")
	}
	failingClient := &soxyClient{
		baseURL: "http://soxy.test", token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrClosedPipe
		})},
	}
	if err := failingClient.request(context.Background(), http.MethodGet, "/", nil, http.StatusOK, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("request(transport failure) error = %v", err)
	}
}

func readySession(id string) map[string]any {
	return map[string]any{
		"id": id, "status": "active", "ready": true,
		"proxy": map[string]any{"scheme": "http", "host": "127.0.0.1", "port": 11001},
	}
}

func writeTestJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
