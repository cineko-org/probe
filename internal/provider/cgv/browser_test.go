package cgv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldBlockResource(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		url          string
		resourceType string
		blocked      bool
	}{
		{"https://cgv.co.kr/", "document", false},
		{"https://cgv.co.kr/api/seats", "xhr", false},
		{"https://cgv.co.kr/api/seats", "fetch", false},
		{"https://cgv.co.kr/app.js", "script", false},
		{"https://cdn.cgv.co.kr/app.js", "script", false},
		{"https://cdn.example/app.js", "script", true},
		{"https://cgv.co.kr/poster.webp", "image", true},
		{"https://www.google-analytics.com/collect", "fetch", true},
	} {
		if got := shouldBlockResource(test.url, test.resourceType); got != test.blocked {
			t.Fatalf("shouldBlockResource(%q, %q) = %t", test.url, test.resourceType, got)
		}
	}
}

func TestPersistentContextOptionsIncludeAssignedProxy(t *testing.T) {
	t.Parallel()
	config := DefaultBrowserConfig()
	config.Proxy = &BrowserProxy{Server: "http://proxy.test:11001", Username: "user", Password: "secret"}
	options := persistentContextOptions(config, "")
	if options.Proxy == nil || options.Proxy.Server != config.Proxy.Server {
		t.Fatalf("proxy options = %+v", options.Proxy)
	}
	if options.Proxy.Username == nil || *options.Proxy.Username != "user" || options.Proxy.Password == nil || *options.Proxy.Password != "secret" {
		t.Fatalf("proxy credentials = %+v", options.Proxy)
	}

	config.Proxy = &BrowserProxy{Server: "socks5://proxy.test:11002"}
	options = persistentContextOptions(config, "")
	if options.Proxy.Username != nil || options.Proxy.Password != nil {
		t.Fatalf("empty proxy credentials = %+v", options.Proxy)
	}
}

func TestPersistentContextOptionsRestoreOnlyConfiguredSession(t *testing.T) {
	t.Parallel()
	config := DefaultBrowserConfig()
	options := persistentContextOptions(config, "")
	if containsString(options.Args, "--restore-last-session") {
		t.Fatal("scan-like browser options restore the previous session")
	}

	config.RestoreSession = true
	options = persistentContextOptions(config, "ko-KR")
	if !containsString(options.Args, "--restore-last-session") {
		t.Fatal("session browser options do not restore the previous session")
	}
	if !containsString(options.IgnoreDefaultArgs, "--no-startup-window") {
		t.Fatal("session browser options still suppress Chrome startup restoration")
	}
	if options.Locale == nil || *options.Locale != "ko-KR" {
		t.Fatalf("session locale = %v", options.Locale)
	}
	if options.TimezoneId == nil || *options.TimezoneId != "Asia/Seoul" {
		t.Fatalf("session time zone = %v", options.TimezoneId)
	}
}

func TestSessionIdentityAndStorageSurviveCleanBrowserRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Playwright Chromium")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(writer, `<title>Cineko session fixture</title>`)
	}))
	defer server.Close()

	config := DefaultBrowserConfig()
	config.ProfileDir = t.TempDir()
	config.ArtifactsDir = t.TempDir()
	config.RestoreSession = true
	config.UserAgentMode = UserAgentSession

	first, err := NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("first NewAdapter() error = %v", err)
	}
	if err := first.navigate(server.URL); err != nil {
		first.Close()
		t.Fatalf("first navigate() error = %v", err)
	}
	if _, err := first.page.Evaluate(`localStorage.setItem('cineko-session', 'preserved')`); err != nil {
		first.Close()
		t.Fatalf("write local storage: %v", err)
	}
	if _, err := first.page.Evaluate(`document.cookie = 'cineko_session=preserved; Path=/'`); err != nil {
		first.Close()
		t.Fatalf("write session cookie: %v", err)
	}
	firstIdentity := first.userAgent
	firstMetadata := first.userAgentMetadata
	firstWebGL := first.webGLIdentity
	first.Close()

	second, err := NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("second NewAdapter() error = %v", err)
	}
	defer second.Close()
	if err := second.navigate(server.URL); err != nil {
		t.Fatalf("second navigate() error = %v", err)
	}
	value, err := second.page.Evaluate(`localStorage.getItem('cineko-session')`)
	if err != nil || value != "preserved" {
		t.Fatalf("restored local storage = %#v, %v", value, err)
	}
	cookies, err := second.page.Evaluate(`document.cookie`)
	if err != nil || !strings.Contains(fmt.Sprint(cookies), "cineko_session=preserved") {
		t.Fatalf("restored session cookie = %#v, %v", cookies, err)
	}
	if !reflect.DeepEqual(second.userAgent, firstIdentity) ||
		!reflect.DeepEqual(second.userAgentMetadata, firstMetadata) ||
		!reflect.DeepEqual(second.webGLIdentity, firstWebGL) {
		t.Fatal("browser identity changed across the clean restart")
	}
}

func TestAdapterCloseHooksSurviveCloseRaces(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{cancelContext: func() {}}
	var calls atomic.Int64
	adapter.AddCloseHook(func() { calls.Add(1) })
	adapter.Close()
	adapter.AddCloseHook(func() { calls.Add(1) })
	adapter.Close()
	if calls.Load() != 2 {
		t.Fatalf("close hook calls = %d", calls.Load())
	}
	for iteration := 0; iteration < 100; iteration++ {
		adapter := &Adapter{cancelContext: func() {}}
		var racedCalls atomic.Int64
		var waitGroup sync.WaitGroup
		waitGroup.Add(2)
		go func() {
			defer waitGroup.Done()
			adapter.AddCloseHook(func() { racedCalls.Add(1) })
		}()
		go func() {
			defer waitGroup.Done()
			adapter.Close()
		}()
		waitGroup.Wait()
		if racedCalls.Load() != 1 {
			t.Fatalf("race iteration %d hook calls = %d", iteration, racedCalls.Load())
		}
	}
}

func TestStealthBootstrapAppliesBeforeNavigation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a local Chrome installation")
	}
	config := DefaultBrowserConfig()
	config.ProfileDir = t.TempDir()
	config.ArtifactsDir = t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(writer, `<canvas id="surface"></canvas>`)
	}))
	defer server.Close()

	adapter, err := NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	defer adapter.Close()
	if err := adapter.navigate(server.URL); err != nil {
		t.Fatalf("navigate() error = %v", err)
	}

	var identity struct {
		WebdriverUndefined   bool     `json:"webdriverUndefined"`
		WebGLVendor          string   `json:"webglVendor"`
		WebGLRenderer        string   `json:"webglRenderer"`
		WebGL2Vendor         string   `json:"webgl2Vendor"`
		WebGL2Renderer       string   `json:"webgl2Renderer"`
		PluginArrayTag       string   `json:"pluginArrayTag"`
		PluginCount          int64    `json:"pluginCount"`
		FirstPluginTag       string   `json:"firstPluginTag"`
		MimeTypeArrayTag     string   `json:"mimeTypeArrayTag"`
		MimeTypeCount        int64    `json:"mimeTypeCount"`
		Languages            []string `json:"languages"`
		Language             string   `json:"language"`
		Vendor               string   `json:"vendor"`
		UserAgent            string   `json:"userAgent"`
		Platform             string   `json:"platform"`
		HardwareConcurrency  int64    `json:"hardwareConcurrency"`
		ChromeApp            bool     `json:"chromeApp"`
		ChromeCSI            bool     `json:"chromeCSI"`
		ChromeLoadTimes      bool     `json:"chromeLoadTimes"`
		ConsolePatched       bool     `json:"consolePatched"`
		PermissionState      string   `json:"permissionState"`
		EventTrustedSpoofed  bool     `json:"eventTrustedSpoofed"`
		H264Support          string   `json:"h264Support"`
		OuterDimensionsValid bool     `json:"outerDimensionsValid"`
		WebGLFunctionNative  bool     `json:"webglFunctionNative"`
		PluginLookupWorks    bool     `json:"pluginLookupWorks"`
		MimeTypeLinkWorks    bool     `json:"mimeTypeLinkWorks"`
		FrameWebdriverHidden bool     `json:"frameWebdriverHidden"`
		FrameLanguages       []string `json:"frameLanguages"`
		FrameElementCoherent bool     `json:"frameElementCoherent"`
		UADataPlatform       string   `json:"uaDataPlatform"`
		UADataBrands         []struct {
			Brand   string `json:"brand"`
			Version string `json:"version"`
		} `json:"uaDataBrands"`
		UADataFullVersions []struct {
			Brand   string `json:"brand"`
			Version string `json:"version"`
		} `json:"uaDataFullVersions"`
	}
	err = adapter.evaluate(`(async () => {
		const canvas = document.querySelector('#surface');
		const context = canvas.getContext('webgl');
		const secondCanvas = document.createElement('canvas');
		const context2 = secondCanvas.getContext('webgl2');
		const video = document.createElement('video');
		const iframe = document.createElement('iframe');
		const frameLoaded = new Promise(resolve => iframe.addEventListener('load', resolve, {once: true}));
		iframe.srcdoc = '<html><body>frame</body></html>';
		document.body.append(iframe);
		await frameLoaded;
		const permission = await navigator.permissions.query({name: 'notifications'});
		const eventTarget = document.createElement('button');
		let eventTrustedSpoofed = false;
		eventTarget.addEventListener('click', event => { eventTrustedSpoofed = event.isTrusted; });
		eventTarget.dispatchEvent(new Event('click'));
		const uaData = await navigator.userAgentData.getHighEntropyValues(['fullVersionList']);
		return {
			webdriverUndefined: navigator.webdriver === undefined,
			webglVendor: context?.getParameter(37445),
			webglRenderer: context?.getParameter(37446),
			webgl2Vendor: context2?.getParameter(37445),
			webgl2Renderer: context2?.getParameter(37446),
			pluginArrayTag: Object.prototype.toString.call(navigator.plugins),
			pluginCount: navigator.plugins.length,
			firstPluginTag: Object.prototype.toString.call(navigator.plugins[0]),
			mimeTypeArrayTag: Object.prototype.toString.call(navigator.mimeTypes),
			mimeTypeCount: navigator.mimeTypes.length,
			languages: Array.from(navigator.languages),
			language: navigator.language,
			vendor: navigator.vendor,
			userAgent: navigator.userAgent,
			platform: navigator.platform,
			hardwareConcurrency: navigator.hardwareConcurrency,
			chromeApp: typeof window.chrome?.app === 'object',
			chromeCSI: typeof window.chrome?.csi === 'function',
			chromeLoadTimes: typeof window.chrome?.loadTimes === 'function',
			consolePatched: console.log.toString().includes('=>') && console.debug.toString().includes('=>') && typeof console.context === 'function',
			permissionState: permission.state,
			eventTrustedSpoofed,
			h264Support: video.canPlayType('video/mp4; codecs="avc1.42E01E"'),
			outerDimensionsValid: window.outerWidth >= window.innerWidth && window.outerHeight >= window.innerHeight,
			webglFunctionNative: WebGLRenderingContext.prototype.getParameter.toString().includes('[native code]'),
			pluginLookupWorks: navigator.plugins.namedItem(navigator.plugins[0].name) === navigator.plugins[0],
			mimeTypeLinkWorks: navigator.mimeTypes[0].enabledPlugin instanceof Plugin,
			frameWebdriverHidden: iframe.contentWindow.navigator.webdriver === undefined,
			frameLanguages: Array.from(iframe.contentWindow.navigator.languages),
			frameElementCoherent: iframe.contentWindow.frameElement === iframe,
			uaDataPlatform: uaData.platform,
			uaDataBrands: uaData.brands,
			uaDataFullVersions: uaData.fullVersionList
		};
	})()`, &identity)
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	if !identity.WebdriverUndefined {
		t.Fatal("navigator.webdriver still exposes browser automation")
	}
	if identity.WebGLVendor != adapter.webGLIdentity.Vendor || identity.WebGLRenderer != adapter.webGLIdentity.Renderer {
		t.Fatalf("WebGL identity = %q/%q", identity.WebGLVendor, identity.WebGLRenderer)
	}
	if identity.WebGL2Vendor != adapter.webGLIdentity.Vendor || identity.WebGL2Renderer != adapter.webGLIdentity.Renderer {
		t.Fatalf("WebGL2 identity = %q/%q", identity.WebGL2Vendor, identity.WebGL2Renderer)
	}
	if runtime.GOARCH == "arm64" && strings.Contains(strings.ToLower(identity.WebGLRenderer), "intel") {
		t.Fatalf("ARM64 browser exposes an Intel WebGL renderer: %q", identity.WebGLRenderer)
	}
	if identity.PluginArrayTag != "[object PluginArray]" || identity.PluginCount == 0 || identity.FirstPluginTag != "[object Plugin]" {
		t.Fatalf("navigator.plugins shape = %q, count %d, item %q", identity.PluginArrayTag, identity.PluginCount, identity.FirstPluginTag)
	}
	if identity.MimeTypeArrayTag != "[object MimeTypeArray]" || identity.MimeTypeCount == 0 {
		t.Fatalf("navigator.mimeTypes shape = %q, count %d", identity.MimeTypeArrayTag, identity.MimeTypeCount)
	}
	if len(identity.Languages) == 0 || identity.Language != identity.Languages[0] {
		t.Fatalf("navigator language/languages = %q/%v", identity.Language, identity.Languages)
	}
	if strings.Contains(identity.UserAgent, "HeadlessChrome") || !strings.Contains(identity.UserAgent, " Chrome/") {
		t.Fatalf("navigator.userAgent = %q", identity.UserAgent)
	}
	if strings.TrimSpace(identity.Platform) == "" {
		t.Fatal("navigator.platform is empty")
	}
	if identity.Vendor != "Google Inc." || identity.HardwareConcurrency != 4 {
		t.Fatalf("navigator vendor/concurrency = %q/%d", identity.Vendor, identity.HardwareConcurrency)
	}
	if !identity.ChromeApp || !identity.ChromeCSI || !identity.ChromeLoadTimes {
		t.Fatalf("window.chrome evasions = app:%t csi:%t loadTimes:%t", identity.ChromeApp, identity.ChromeCSI, identity.ChromeLoadTimes)
	}
	if identity.ConsolePatched || identity.PermissionState != "denied" || identity.EventTrustedSpoofed {
		t.Fatalf("passive chrome stealth = consolePatched:%t permission:%q eventTrustedSpoofed:%t", identity.ConsolePatched, identity.PermissionState, identity.EventTrustedSpoofed)
	}
	if identity.H264Support == "" {
		t.Fatal("H.264 codec support is still empty")
	}
	if !identity.OuterDimensionsValid {
		t.Fatal("window.outerWidth/outerHeight are inconsistent with inner dimensions")
	}
	if !identity.WebGLFunctionNative {
		t.Fatal("patched WebGL getParameter does not retain a native function shape")
	}
	if !identity.PluginLookupWorks || !identity.MimeTypeLinkWorks {
		t.Fatalf("plugin/mime type relationships = lookup:%t link:%t", identity.PluginLookupWorks, identity.MimeTypeLinkWorks)
	}
	if !identity.FrameWebdriverHidden || !reflect.DeepEqual(identity.FrameLanguages, identity.Languages) || !identity.FrameElementCoherent {
		t.Fatalf("iframe evasions = webdriver:%t languages:%v frameElement:%t", identity.FrameWebdriverHidden, identity.FrameLanguages, identity.FrameElementCoherent)
	}
	if identity.UADataPlatform != browserUADataPlatform() {
		t.Fatalf("navigator.userAgentData.platform = %q", identity.UADataPlatform)
	}
	assertChromeBrandVersion(t, identity.UADataBrands, adapter.userAgent.Major)
	assertChromeBrandVersion(t, identity.UADataFullVersions, adapter.userAgent.FullVersion)
}

func assertChromeBrandVersion(t *testing.T, brands []struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}, expected string) {
	t.Helper()
	for _, brand := range brands {
		if brand.Brand == "Chromium" {
			if brand.Version != expected {
				t.Fatalf("Chromium brand version = %q, want %q", brand.Version, expected)
			}
			return
		}
	}
	t.Fatalf("Chromium brand is missing: %+v", brands)
}

func TestBrowserResourceRouting(t *testing.T) {
	config := localBrowserTestConfig(t)
	var mu sync.Mutex
	hits := make(map[string]int)
	var documentUserAgent string
	var documentAcceptLanguage string
	var documentSecCHUA string
	var documentSecCHUAPlatform string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		hits[request.URL.Path]++
		if request.URL.Path == "/" {
			documentUserAgent = request.Header.Get("User-Agent")
			documentAcceptLanguage = request.Header.Get("Accept-Language")
			documentSecCHUA = request.Header.Get("Sec-Ch-Ua")
			documentSecCHUAPlatform = request.Header.Get("Sec-Ch-Ua-Platform")
		}
		mu.Unlock()
		switch request.URL.Path {
		case "/":
			writer.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(writer, `<html><head>
				<link rel="stylesheet" href="/site.css">
				<script src="/external.js"></script>
			</head><body><img src="/poster.png"><script>
				Promise.all([
					fetch('/api/data').then(response => response.text()),
					fetch('/analytics/collect').catch(() => 'blocked')
				]).then(() => window.requestsSettled = true);
			</script></body></html>`)
		default:
			_, _ = fmt.Fprint(writer, "ok")
		}
	}))
	defer server.Close()

	adapter, err := NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	defer adapter.Close()
	if err := adapter.navigate(server.URL); err != nil {
		t.Fatalf("navigate() error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var settled bool
		if err := adapter.evaluate("window.requestsSettled === true", &settled); err == nil && settled {
			break
		}
		_ = adapter.wait(20 * time.Millisecond)
	}

	mu.Lock()
	if hits["/"] != 1 || hits["/api/data"] != 1 {
		mu.Unlock()
		t.Fatalf("allowed request hits = %#v", hits)
	}
	for _, path := range []string{"/site.css", "/external.js", "/poster.png", "/analytics/collect"} {
		if hits[path] != 0 {
			mu.Unlock()
			t.Fatalf("blocked request %s reached the server: %#v", path, hits)
		}
	}
	mu.Unlock()
	if adapter.blockedRequests.Load() < 4 {
		t.Fatalf("blocked request count = %d, want at least 4", adapter.blockedRequests.Load())
	}
	if strings.Contains(documentUserAgent, "HeadlessChrome") || !strings.Contains(documentUserAgent, " Chrome/") {
		t.Fatalf("document User-Agent = %q", documentUserAgent)
	}
	if !strings.Contains(documentSecCHUA, `"Chromium";v="`+adapter.userAgent.Major+`"`) {
		t.Fatalf("document Sec-Ch-Ua = %q", documentSecCHUA)
	}
	if documentSecCHUAPlatform != strconv.Quote(browserUADataPlatform()) {
		t.Fatalf("document Sec-Ch-Ua-Platform = %q", documentSecCHUAPlatform)
	}
	var browserLanguage string
	if err := adapter.evaluate("navigator.language", &browserLanguage); err != nil {
		t.Fatalf("read navigator.language: %v", err)
	}
	primaryHeaderLanguage, _, _ := strings.Cut(documentAcceptLanguage, ",")
	if !strings.EqualFold(strings.TrimSpace(primaryHeaderLanguage), browserLanguage) {
		t.Fatalf("document/browser language = %q/%q", documentAcceptLanguage, browserLanguage)
	}
}

func TestBrowserPoolUsesOnePageAndRestartsPerTask(t *testing.T) {
	config := localBrowserTestConfig(t)
	pool, err := NewBrowserPool(config)
	if err != nil {
		t.Fatalf("NewBrowserPool() error = %v", err)
	}
	defer pool.Close()

	first, err := pool.NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("first NewAdapter() error = %v", err)
	}
	if pages := len(first.browserContext.Pages()); pages != 1 {
		t.Fatalf("first browser page count = %d, want 1", pages)
	}
	firstBrowser := first.browserContext.Browser()

	secondResult := make(chan struct {
		adapter *Adapter
		err     error
	}, 1)
	go func() {
		adapter, err := pool.NewAdapter(context.Background(), config)
		secondResult <- struct {
			adapter *Adapter
			err     error
		}{adapter: adapter, err: err}
	}()
	select {
	case <-secondResult:
		t.Fatal("second browser task started before the first process closed")
	case <-time.After(150 * time.Millisecond):
	}
	first.Close()

	var second *Adapter
	select {
	case result := <-secondResult:
		if result.err != nil {
			t.Fatalf("second NewAdapter() error = %v", result.err)
		}
		second = result.adapter
	case <-time.After(10 * time.Second):
		t.Fatal("second browser task did not start after the first process closed")
	}
	defer second.Close()
	if pages := len(second.browserContext.Pages()); pages != 1 {
		t.Fatalf("second browser page count = %d, want 1", pages)
	}
	if firstBrowser == second.browserContext.Browser() {
		t.Fatal("browser process was reused across logical tasks")
	}
}

func TestRandomizedScanIdentityStaysFixedForBrowserProcess(t *testing.T) {
	config := localBrowserTestConfig(t)
	config.UserAgentMode = UserAgentRandomizedScan
	var mu sync.Mutex
	requestUserAgents := make([]string, 0, 2)
	requestClientHints := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requestUserAgents = append(requestUserAgents, request.Header.Get("User-Agent"))
		requestClientHints = append(requestClientHints, request.Header.Get("Sec-Ch-Ua"))
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(writer, `<html><body>scan</body></html>`)
	}))
	defer server.Close()

	adapter, err := NewAdapter(context.Background(), config)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	defer adapter.Close()
	for _, path := range []string{"/first", "/second"} {
		if err := adapter.navigate(server.URL + path); err != nil {
			t.Fatalf("navigate %s: %v", path, err)
		}
	}
	var identity struct {
		UserAgent    string `json:"userAgent"`
		Architecture string `json:"architecture"`
		FullVersions []struct {
			Brand   string `json:"brand"`
			Version string `json:"version"`
		} `json:"fullVersions"`
	}
	if err := adapter.evaluate(`(async () => {
		const data = await navigator.userAgentData.getHighEntropyValues(['architecture', 'fullVersionList']);
		return {userAgent: navigator.userAgent, architecture: data.architecture, fullVersions: data.fullVersionList};
	})()`, &identity); err != nil {
		t.Fatalf("read randomized scan identity: %v", err)
	}
	if identity.UserAgent != adapter.userAgent.Value || identity.Architecture != browserArchitecture() {
		t.Fatalf("randomized scan JS identity = %+v, selected = %+v", identity, adapter.userAgent)
	}
	assertChromeBrandVersion(t, identity.FullVersions, adapter.userAgent.FullVersion)

	mu.Lock()
	defer mu.Unlock()
	if len(requestUserAgents) != 2 || len(requestClientHints) != 2 {
		t.Fatalf("scan request identities = UA:%v UA-CH:%v", requestUserAgents, requestClientHints)
	}
	for index := range requestUserAgents {
		if requestUserAgents[index] != adapter.userAgent.Value ||
			!strings.Contains(requestClientHints[index], `"Chromium";v="`+adapter.userAgent.Major+`"`) {
			t.Fatalf("scan request %d identity = %q / %q", index, requestUserAgents[index], requestClientHints[index])
		}
	}
}

func TestLiveNowSecureStealth(t *testing.T) {
	if os.Getenv("CINEKO_LIVE_STEALTH") != "1" {
		t.Skip("set CINEKO_LIVE_STEALTH=1 to run the external detector smoke test")
	}
	config := localBrowserTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adapter, err := NewAdapter(ctx, config)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	defer adapter.Close()
	if err := adapter.navigate("https://nowsecure.nl/"); err != nil {
		t.Fatalf("navigate nowsecure.nl: %v", err)
	}
	var observation struct {
		Secure              bool   `json:"secure"`
		WebdriverUndefined  bool   `json:"webdriverUndefined"`
		UserAgent           string `json:"userAgent"`
		ChromeApp           bool   `json:"chromeApp"`
		ConsolePatched      bool   `json:"consolePatched"`
		PluginCount         int64  `json:"pluginCount"`
		HardwareConcurrency int64  `json:"hardwareConcurrency"`
		UADataPlatform      string `json:"uaDataPlatform"`
		UADataArchitecture  string `json:"uaDataArchitecture"`
		UADataChromeVersion string `json:"uaDataChromeVersion"`
		WebGLRenderer       string `json:"webglRenderer"`
		NotificationState   string `json:"notificationState"`
		PermissionState     string `json:"permissionState"`
	}
	if err := adapter.evaluate(`(async () => {
		const uaData = await navigator.userAgentData.getHighEntropyValues(['architecture', 'fullVersionList']);
		const chromeVersion = uaData.fullVersionList.find(entry => entry.brand === 'Chromium')?.version || '';
		const canvas = document.createElement('canvas');
		const webgl = canvas.getContext('webgl');
		const permission = await navigator.permissions.query({name: 'notifications'});
		return {
			secure: location.protocol === 'https:',
			webdriverUndefined: navigator.webdriver === undefined,
			userAgent: navigator.userAgent,
			chromeApp: typeof window.chrome?.app === 'object',
			consolePatched: console.log.toString().includes('=>') && console.debug.toString().includes('=>') && typeof console.context === 'function',
			pluginCount: navigator.plugins.length,
			hardwareConcurrency: navigator.hardwareConcurrency,
			uaDataPlatform: uaData.platform,
			uaDataArchitecture: uaData.architecture,
			uaDataChromeVersion: chromeVersion,
			webglRenderer: webgl?.getParameter(37446),
			notificationState: Notification.permission,
			permissionState: permission.state
		};
	})()`, &observation); err != nil {
		t.Fatalf("read nowsecure.nl identity: %v", err)
	}
	expectedArchitecture := "x86"
	if runtime.GOARCH == "arm64" {
		expectedArchitecture = "arm"
	}
	if !observation.Secure || !observation.WebdriverUndefined || strings.Contains(observation.UserAgent, "HeadlessChrome") ||
		!observation.ChromeApp || !observation.ConsolePatched || observation.PluginCount == 0 || observation.HardwareConcurrency != 4 ||
		observation.UADataPlatform != browserUADataPlatform() || observation.UADataArchitecture != expectedArchitecture ||
		observation.UADataChromeVersion != adapter.userAgent.FullVersion ||
		observation.WebGLRenderer != adapter.webGLIdentity.Renderer ||
		observation.PermissionState != "denied" {
		t.Fatalf("nowsecure.nl stealth observation = %+v", observation)
	}
	t.Logf("nowsecure.nl stealth observation = %+v", observation)
}

func localBrowserTestConfig(t *testing.T) BrowserConfig {
	t.Helper()
	config := DefaultBrowserConfig()
	config.ProfileDir = t.TempDir()
	config.ArtifactsDir = t.TempDir()
	return config
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
