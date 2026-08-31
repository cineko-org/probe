package cgv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserNetworkCapturePersists429AndStopsFollowupRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Launcher Playwright runtime and local Chrome")
	}
	chromePath := strings.TrimSpace(os.Getenv("CINEKO_CHROME_PATH"))
	if chromePath == "" && runtime.GOOS == "darwin" {
		chromePath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	if _, err := os.Stat(chromePath); err != nil { // #nosec G703 -- the executable path is explicit test configuration.
		t.Skipf("local Chrome unavailable: %v", err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":"fixture rate limit"}`))
	}))
	defer server.Close()

	artifacts := t.TempDir()
	config := DefaultBrowserConfig()
	config.ChromePath = chromePath
	config.ProfileDir = t.TempDir()
	config.ArtifactsDir = artifacts
	config.BlockResources = false
	adapter, err := NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	defer adapter.Close()

	if err := adapter.navigate(server.URL + "/limited?round=1"); !errors.Is(err, ErrProviderThrottled) {
		t.Fatalf("first navigate error = %v, want ErrProviderThrottled", err)
	}
	if err := adapter.providerRateLimitError(server.URL); !errors.Is(err, ErrProviderThrottled) {
		t.Fatalf("circuit error = %v, want ErrProviderThrottled", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider request count = %d, want 1", got)
	}

	var manifests []string
	deadline := time.Now().Add(2 * time.Second)
	for len(manifests) == 0 && time.Now().Before(deadline) {
		manifests, err = filepath.Glob(filepath.Join(artifacts, "network", "exchanges", "*", "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(manifests) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(manifests) != 1 {
		t.Fatalf("network manifest count = %d, want 1", len(manifests))
	}
	manifest, err := os.ReadFile(manifests[0]) // #nosec G304 -- glob is constrained to the temporary test artifact root.
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`"status": 429`, `"name": "Retry-After"`, `"url": "` + server.URL + `/limited?round=1"`,
		`"representation": "decoded"`,
	} {
		if !strings.Contains(string(manifest), expected) {
			t.Fatalf("manifest missing %q:\n%s", expected, manifest)
		}
	}
}
