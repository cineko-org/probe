package cgv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBrowserRuntimePreflightRejectsInvalidInputs(t *testing.T) {
	if err := PreflightBrowserRuntime(nil, ""); err == nil { //nolint:staticcheck // Explicit nil boundary.
		t.Fatal("nil preflight context accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PreflightBrowserRuntime(cancelled, ""); err == nil {
		t.Fatal("cancelled preflight context accepted")
	}
	t.Setenv("CINEKO_PLAYWRIGHT_DRIVER_PATH", "")
	if err := PreflightBrowserRuntime(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing configured Chromium accepted")
	}
}

func TestLauncherPlaywrightDriverMustBeComplete(t *testing.T) {
	t.Parallel()
	driver := t.TempDir()
	if err := validatePlaywrightDriver(driver); err == nil {
		t.Fatal("validatePlaywrightDriver(incomplete) error = nil")
	}
	node := "node"
	if runtime.GOOS == "windows" {
		node = "node.exe"
	}
	if err := os.WriteFile(filepath.Join(driver, node), []byte("node"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(driver, "package"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(driver, "package", "cli.js"), []byte("driver"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePlaywrightDriver(driver); err != nil {
		t.Fatalf("validatePlaywrightDriver() error = %v", err)
	}
}

func TestConfiguredBrowserExecutableMustExist(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "chromium")
	if _, err := resolveBrowserExecutable(nil, path); err == nil {
		t.Fatal("resolveBrowserExecutable(missing) error = nil")
	}
	if err := os.WriteFile(path, []byte("browser"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveBrowserExecutable(nil, path)
	if err != nil || resolved != path {
		t.Fatalf("resolveBrowserExecutable() = %q, %v", resolved, err)
	}
}
