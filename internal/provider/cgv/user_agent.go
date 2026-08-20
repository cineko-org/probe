package cgv

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/mxschmitt/playwright-go"
)

type UserAgentMode string

const (
	UserAgentSession        UserAgentMode = "session"
	UserAgentRandomizedScan UserAgentMode = "randomized-scan"
)

type browserUserAgent struct {
	Value       string
	Major       string
	FullVersion string
}

type uaBrandVersion struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

var chromeBuildCache sync.Map

// These exact Stable builds were verified for each runtime against Google's
// public Chrome VersionHistory API on 2026-08-12. Legacy UA strings are reduced
// to the major version, while UA-CH receives a full version from the same OS
// family. Unknown runtime families use only the installed browser build.
var verifiedScanChromeVersions = map[string][]string{
	"linux":     {"151.0.7922.137", "150.0.7871.186", "149.0.7827.200"},
	"mac":       {"151.0.7922.138", "150.0.7871.189", "149.0.7827.201"},
	"mac_arm64": {"151.0.7922.138", "150.0.7871.189", "149.0.7827.201"},
	"win64":     {"151.0.7922.138", "150.0.7871.189", "149.0.7827.201"},
}

func selectBrowserUserAgent(chromePath string, mode UserAgentMode, random io.Reader) (browserUserAgent, error) {
	installedVersion, err := installedChromeVersion(chromePath)
	if err != nil {
		return browserUserAgent{}, err
	}
	if mode == "" {
		mode = UserAgentSession
	}
	if mode == UserAgentSession {
		return makeBrowserUserAgent(installedVersion)
	}
	if mode != UserAgentRandomizedScan {
		return browserUserAgent{}, fmt.Errorf("unsupported user agent mode %q", mode)
	}
	installedMajor, err := chromeMajor(installedVersion)
	if err != nil {
		return browserUserAgent{}, err
	}
	candidates := []string{installedVersion}
	seen := map[string]struct{}{installedVersion: {}}
	for _, version := range verifiedScanChromeVersions[scanVersionPlatform(runtime.GOOS, runtime.GOARCH)] {
		major, versionErr := chromeMajor(version)
		if versionErr != nil || major > installedMajor || installedMajor-major > 2 {
			continue
		}
		if _, exists := seen[version]; exists {
			continue
		}
		seen[version] = struct{}{}
		candidates = append(candidates, version)
	}
	if random == nil {
		random = rand.Reader
	}
	index, err := rand.Int(random, big.NewInt(int64(len(candidates))))
	if err != nil {
		return browserUserAgent{}, fmt.Errorf("select randomized scan user agent: %w", err)
	}
	return makeBrowserUserAgent(candidates[index.Int64()])
}

func scanVersionPlatform(goos, goarch string) string {
	switch goos {
	case "darwin":
		if goarch == "arm64" {
			return "mac_arm64"
		}
		return "mac"
	case "linux":
		return "linux"
	case "windows":
		return "win64"
	default:
		return ""
	}
}

func makeBrowserUserAgent(fullVersion string) (browserUserAgent, error) {
	major, err := chromeMajor(fullVersion)
	if err != nil {
		return browserUserAgent{}, err
	}
	majorString := strconv.Itoa(major)
	return browserUserAgent{
		Major:       majorString,
		FullVersion: fullVersion,
	}, nil
}

func readNativeReducedUserAgent(page playwright.Page, major string) (string, error) {
	value, err := page.Evaluate(`navigator.userAgent`)
	if err != nil {
		return "", fmt.Errorf("read native browser user agent: %w", err)
	}
	native, ok := value.(string)
	if !ok || strings.TrimSpace(native) == "" {
		return "", errors.New("native browser user agent is empty")
	}
	fields := strings.Fields(native)
	replaced := false
	for index, field := range fields {
		prefix := "Chrome/"
		if strings.HasPrefix(field, "HeadlessChrome/") {
			prefix = "HeadlessChrome/"
		}
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		fields[index] = "Chrome/" + major + ".0.0.0"
		replaced = true
		break
	}
	if !replaced {
		return "", errors.New("native browser user agent has no Chrome product token")
	}
	return strings.Join(fields, " "), nil
}

func installedChromeVersion(chromePath string) (string, error) {
	if cached, ok := chromeBuildCache.Load(chromePath); ok {
		version, valid := cached.(string)
		if !valid {
			return "", errors.New("read Chrome version: invalid cache value")
		}
		return version, nil
	}
	output, err := exec.CommandContext(context.Background(), chromePath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("read Chrome version: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", errors.New("read Chrome version: empty output")
	}
	version := fields[len(fields)-1]
	if _, err := chromeMajor(version); err != nil {
		return "", fmt.Errorf("read Chrome version: %w", err)
	}
	chromeBuildCache.Store(chromePath, version)
	return version, nil
}

func chromeMajor(version string) (int, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 4 {
		return 0, fmt.Errorf("invalid Chrome version %q", version)
	}
	for _, part := range parts {
		if part == "" {
			return 0, fmt.Errorf("invalid Chrome version %q", version)
		}
		if _, err := strconv.Atoi(part); err != nil {
			return 0, fmt.Errorf("invalid Chrome version %q", version)
		}
	}
	major, _ := strconv.Atoi(parts[0])
	if major <= 0 {
		return 0, fmt.Errorf("invalid Chrome version %q", version)
	}
	return major, nil
}

type userAgentBootstrapIdentity struct {
	Brands          []uaBrandVersion `json:"brands"`
	FullVersionList []uaBrandVersion `json:"fullVersionList"`
	Platform        string           `json:"platform"`
	PlatformVersion string           `json:"platformVersion"`
	Architecture    string           `json:"architecture"`
	Bitness         string           `json:"bitness"`
	Mobile          bool             `json:"mobile"`
	Wow64           bool             `json:"wow64"`
	FormFactors     []string         `json:"formFactors"`
}

func browserUserAgentBootstrap(identity userAgentBootstrapIdentity) (string, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode browser UA identity: %w", err)
	}
	return `(() => {
	const identity = ` + string(encoded) + `;
	const prototype = globalThis.NavigatorUAData?.prototype;
	if (!prototype) return;
	const replaceGetter = (name, value) => {
		const descriptor = Object.getOwnPropertyDescriptor(prototype, name);
		if (!descriptor?.get) return;
		const getter = new Proxy(descriptor.get, {apply: () => value});
		Object.defineProperty(prototype, name, {...descriptor, get: getter});
	};
	const brands = Object.freeze(identity.brands.map(value => Object.freeze({...value})));
	replaceGetter('brands', brands);
	replaceGetter('platform', identity.platform);
	replaceGetter('mobile', identity.mobile);
	const getHighEntropyValues = prototype.getHighEntropyValues;
	const patchedGetHighEntropyValues = new Proxy(getHighEntropyValues, {
		apply(target, thisArg, args) {
			const requested = new Set(args?.[0] || []);
			return Reflect.apply(target, thisArg, args).then(result => {
				result.brands = brands.map(value => ({...value}));
				result.platform = identity.platform;
				result.mobile = identity.mobile;
				const highEntropy = {
					fullVersionList: identity.fullVersionList.map(value => ({...value})),
					platformVersion: identity.platformVersion,
					architecture: identity.architecture,
					bitness: identity.bitness,
					model: '',
					wow64: identity.wow64,
					formFactors: [...identity.formFactors]
				};
				for (const [key, value] of Object.entries(highEntropy)) {
					if (requested.has(key) || Object.prototype.hasOwnProperty.call(result, key)) result[key] = value;
				}
				return result;
			});
		}
	});
	Object.defineProperty(prototype, 'getHighEntropyValues', {
		...Object.getOwnPropertyDescriptor(prototype, 'getHighEntropyValues'),
		value: patchedGetHighEntropyValues
	});
})();`, nil
}

func browserArchitecture() string {
	if runtime.GOARCH == "arm64" {
		return "arm"
	}
	return "x86"
}

func readNativeUserAgentIdentity(
	page playwright.Page,
	selected browserUserAgent,
) (userAgentBootstrapIdentity, error) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("<!doctype html><title>Cineko browser identity</title>"))
	}))
	defer origin.Close()
	if _, err := page.Goto(origin.URL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateDomcontentloaded}); err != nil {
		return userAgentBootstrapIdentity{}, fmt.Errorf("open local browser identity origin: %w", err)
	}
	value, err := page.Evaluate(`navigator.userAgentData.getHighEntropyValues([
		'architecture', 'bitness', 'formFactors', 'fullVersionList', 'platformVersion', 'wow64'
	])`)
	if err != nil {
		return userAgentBootstrapIdentity{}, fmt.Errorf("read native browser UA metadata: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return userAgentBootstrapIdentity{}, fmt.Errorf("encode native browser UA metadata: %w", err)
	}
	var identity userAgentBootstrapIdentity
	if err := json.Unmarshal(encoded, &identity); err != nil {
		return userAgentBootstrapIdentity{}, fmt.Errorf("decode native browser UA metadata: %w", err)
	}
	if !replaceBrowserProductVersions(identity.Brands, selected.Major) ||
		!replaceBrowserProductVersions(identity.FullVersionList, selected.FullVersion) {
		return userAgentBootstrapIdentity{}, errors.New("native browser UA metadata has no Chromium brand")
	}
	if strings.TrimSpace(identity.Platform) == "" ||
		strings.TrimSpace(identity.Architecture) == "" || strings.TrimSpace(identity.Bitness) == "" {
		return userAgentBootstrapIdentity{}, errors.New("native browser UA metadata is incomplete")
	}
	return identity, nil
}

func replaceBrowserProductVersions(brands []uaBrandVersion, version string) bool {
	foundChromium := false
	for index := range brands {
		switch brands[index].Brand {
		case "Chromium":
			foundChromium = true
			brands[index].Version = version
		case "Google Chrome":
			brands[index].Version = version
		}
	}
	return foundChromium
}

func openBrowserIdentitySession(
	page playwright.Page,
	selected browserUserAgent,
	identity userAgentBootstrapIdentity,
) (playwright.CDPSession, error) {
	session, err := page.Context().NewCDPSession(page)
	if err != nil {
		return nil, fmt.Errorf("open browser identity session: %w", err)
	}
	_, err = session.Send("Emulation.setUserAgentOverride", map[string]any{
		"userAgent": selected.Value,
		"userAgentMetadata": map[string]any{
			"brands":          identity.Brands,
			"fullVersionList": identity.FullVersionList,
			"platform":        identity.Platform,
			"platformVersion": identity.PlatformVersion,
			"architecture":    identity.Architecture,
			"bitness":         identity.Bitness,
			"model":           "",
			"mobile":          identity.Mobile,
			"wow64":           identity.Wow64,
			"formFactors":     identity.FormFactors,
		},
	})
	if err != nil {
		_ = session.Detach()
		return nil, fmt.Errorf("apply browser user agent metadata: %w", err)
	}
	return session, nil
}

func browserUADataPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

func applyUserAgentHeaders(
	headers map[string]string,
	selected browserUserAgent,
	identity userAgentBootstrapIdentity,
) {
	headers["user-agent"] = selected.Value
	headers["sec-ch-ua"] = formatBrandHeader(identity.Brands)
	headers["sec-ch-ua-mobile"] = "?0"
	headers["sec-ch-ua-platform"] = strconv.Quote(identity.Platform)
	conditional := map[string]string{
		"sec-ch-ua-full-version-list": formatBrandHeader(identity.FullVersionList),
		"sec-ch-ua-full-version":      strconv.Quote(selected.FullVersion),
		"sec-ch-ua-platform-version":  strconv.Quote(identity.PlatformVersion),
		"sec-ch-ua-arch":              strconv.Quote(identity.Architecture),
		"sec-ch-ua-bitness":           strconv.Quote(identity.Bitness),
		"sec-ch-ua-model":             `""`,
		"sec-ch-ua-wow64":             "?0",
		"sec-ch-ua-form-factors":      `"Desktop"`,
	}
	for name, value := range conditional {
		if _, requested := headers[name]; requested {
			headers[name] = value
		}
	}
}

func formatBrandHeader(brands []uaBrandVersion) string {
	parts := make([]string, 0, len(brands))
	for _, brand := range brands {
		parts = append(parts, strconv.Quote(brand.Brand)+`;v=`+strconv.Quote(brand.Version))
	}
	return strings.Join(parts, ", ")
}
