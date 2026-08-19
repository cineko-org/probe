package cgv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mxschmitt/playwright-go"
)

const browserPreflightTimeout = 20_000

// PreflightBrowserRuntime proves that the configured Playwright driver can
// start the configured Chromium binary. It does not navigate or use any Probe
// credential, profile, proxy lease, or browser identity state.
func PreflightBrowserRuntime(ctx context.Context, configuredExecutable string) error {
	if ctx == nil {
		return errors.New("browser preflight context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pw, err := startPlaywright()
	if err != nil {
		return fmt.Errorf("start Playwright preflight: %w", err)
	}
	defer func() { _ = pw.Stop() }()
	executable, err := resolveBrowserExecutable(pw, configuredExecutable)
	if err != nil {
		return err
	}
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		ExecutablePath: playwright.String(executable),
		Headless:       playwright.Bool(true),
		Timeout:        playwright.Float(browserPreflightTimeout),
	})
	if err != nil {
		return fmt.Errorf("launch Chromium preflight: %w", err)
	}
	defer func() { _ = browser.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	page, err := browser.NewPage()
	if err != nil {
		return fmt.Errorf("create Chromium preflight page: %w", err)
	}
	if _, err := page.Evaluate("1 + 1"); err != nil {
		return fmt.Errorf("evaluate Chromium preflight page: %w", err)
	}
	return ctx.Err()
}

func startPlaywright() (*playwright.Playwright, error) {
	driverDirectory := strings.TrimSpace(os.Getenv("CINEKO_PLAYWRIGHT_DRIVER_PATH"))
	if driverDirectory == "" {
		return playwright.Run()
	}
	if err := validatePlaywrightDriver(driverDirectory); err != nil {
		return nil, err
	}
	return playwright.Run(&playwright.RunOptions{DriverDirectory: driverDirectory})
}

func resolveBrowserExecutable(pw *playwright.Playwright, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return validateBrowserExecutable(configured, "configured")
	}
	if pw == nil || pw.Chromium == nil {
		return "", errors.New("playwright Chromium is unavailable")
	}
	return validateBrowserExecutable(pw.Chromium.ExecutablePath(), "managed")
}

func validatePlaywrightDriver(driverDirectory string) error {
	node := "node"
	if runtime.GOOS == "windows" {
		node = "node.exe"
	}
	for _, required := range []string{
		filepath.Join(driverDirectory, node),
		filepath.Join(driverDirectory, "package", "cli.js"),
	} {
		if _, err := os.Stat(required); err != nil { // #nosec G703 -- Launcher provides and verifies this runtime root.
			return fmt.Errorf("launcher Playwright driver is incomplete at %q: %w", required, err)
		}
	}
	return nil
}

func validateBrowserExecutable(path, source string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." {
		return "", fmt.Errorf("%s Chromium executable path is empty", source)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s Chromium executable %q: %w", source, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s Chromium executable %q is not a file", source, path)
	}
	return path, nil
}
