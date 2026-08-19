package cgv

import (
	"bytes"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

func TestSessionUserAgentUsesInstalledChromeBuild(t *testing.T) {
	userAgent, err := makeBrowserUserAgent("151.0.7922.76")
	if err != nil {
		t.Fatal(err)
	}
	if userAgent.Major != "151" || userAgent.FullVersion != "151.0.7922.76" {
		t.Fatalf("user agent version = %+v", userAgent)
	}
	if userAgent.Value != "" {
		t.Fatalf("user agent must use native browser platform, got %q", userAgent.Value)
	}
}

type userAgentPageStub struct {
	playwright.Page
	value any
	err   error
}

func (stub userAgentPageStub) Evaluate(string, ...any) (any, error) {
	return stub.value, stub.err
}

func TestNativeUserAgentPreservesPlatformAndReducesVersion(t *testing.T) {
	value, err := readNativeReducedUserAgent(userAgentPageStub{value: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/151.0.7922.76 Safari/537.36"}, "150")
	if err != nil {
		t.Fatal(err)
	}
	want := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	if value != want {
		t.Fatalf("reduced user agent = %q, want %q", value, want)
	}
}

func TestNativeUserAgentRejectsInvalidBrowserValue(t *testing.T) {
	for _, value := range []any{nil, "", "Mozilla/5.0 Firefox/151.0"} {
		if _, err := readNativeReducedUserAgent(userAgentPageStub{value: value}, "151"); err == nil {
			t.Fatalf("readNativeReducedUserAgent(%v) error = nil", value)
		}
	}
}

func TestRandomizedScanUserAgentUsesOnlyVerifiedCompatibleBuilds(t *testing.T) {
	platform := scanVersionPlatform(runtime.GOOS, runtime.GOARCH)
	original := verifiedScanChromeVersions
	verifiedScanChromeVersions = map[string][]string{platform: {
		"151.0.7922.109",
		"150.0.7871.189",
		"149.0.7827.201",
		"148.0.7778.168",
		"152.0.8000.1",
	}}
	t.Cleanup(func() { verifiedScanChromeVersions = original })
	chromeBuildCache.Store("test-chrome", "151.0.7922.76")
	t.Cleanup(func() { chromeBuildCache.Delete("test-chrome") })

	seen := make(map[string]struct{})
	for _, randomByte := range []byte{0, 1, 2, 3} {
		selected, err := selectBrowserUserAgent("test-chrome", UserAgentRandomizedScan, bytes.NewReader([]byte{randomByte}))
		if err != nil {
			t.Fatal(err)
		}
		if selected.Major != strings.Split(selected.FullVersion, ".")[0] {
			t.Fatalf("UA/UA-CH version mismatch = %+v", selected)
		}
		if selected.Major != "149" && selected.Major != "150" && selected.Major != "151" {
			t.Fatalf("unverified or incompatible scan user agent = %+v", selected)
		}
		seen[selected.FullVersion] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("randomized selection did not vary: %v", seen)
	}
}

func TestScanVersionPlatformMatchesRuntime(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		goos, goarch, want string
	}{
		"linux amd64":  {goos: "linux", goarch: "amd64", want: "linux"},
		"linux arm64":  {goos: "linux", goarch: "arm64", want: "linux"},
		"darwin amd64": {goos: "darwin", goarch: "amd64", want: "mac"},
		"darwin arm64": {goos: "darwin", goarch: "arm64", want: "mac_arm64"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := scanVersionPlatform(test.goos, test.goarch); got != test.want {
				t.Fatalf("scanVersionPlatform(%q, %q) = %q, want %q", test.goos, test.goarch, got, test.want)
			}
		})
	}
}

func TestBrowserProductBrandVersionsStayCoherent(t *testing.T) {
	t.Parallel()
	brands := []uaBrandVersion{
		{Brand: "Not A Brand", Version: "99"},
		{Brand: "Chromium", Version: "151.0.0.0"},
		{Brand: "Google Chrome", Version: "151.0.0.0"},
	}
	want := []uaBrandVersion{
		{Brand: "Not A Brand", Version: "99"},
		{Brand: "Chromium", Version: "149.0.7827.201"},
		{Brand: "Google Chrome", Version: "149.0.7827.201"},
	}
	if !replaceBrowserProductVersions(brands, "149.0.7827.201") || !reflect.DeepEqual(brands, want) {
		t.Fatalf("browser product brands = %+v, want %+v", brands, want)
	}
	if replaceBrowserProductVersions([]uaBrandVersion{{Brand: "Google Chrome", Version: "151"}}, "149") {
		t.Fatal("metadata without the Chromium engine brand was accepted")
	}
}

func TestInvalidUserAgentModeIsRejected(t *testing.T) {
	chromeBuildCache.Store("test-chrome", "151.0.7922.76")
	t.Cleanup(func() { chromeBuildCache.Delete("test-chrome") })
	if _, err := selectBrowserUserAgent("test-chrome", UserAgentMode("per-request"), bytes.NewReader([]byte{0})); err == nil {
		t.Fatal("invalid per-request UA rotation was accepted")
	}
}
