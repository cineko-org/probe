package cgv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mxschmitt/playwright-go"
)

const (
	homeURL          = "https://cgv.co.kr/"
	loginURL         = "https://cgv.co.kr/mem/login?returnUrl=%2F"
	bookingCinemaURL = "https://cgv.co.kr/cnm/movieBook/cinema"
)

var (
	ErrAuthenticationRequired = errors.New("CGV authentication is required")
	ErrUIContractChanged      = errors.New("CGV UI contract changed")
	ErrCaptchaRequired        = errors.New("manual CAPTCHA entry is required")
)

type BrowserConfig struct {
	ChromePath     string
	ProfileDir     string
	ArtifactsDir   string
	Headless       bool
	StartMinimized bool
	RestoreSession bool
	BlockResources bool
	UserAgentMode  UserAgentMode
	Locale         string
	TimeZone       string
	Proxy          *BrowserProxy
	Capacity       int
}

// BrowserProxy is the proxy identity assigned to one browser process. Secrets
// stay separate from Server so they are never embedded in URLs or logs.
type BrowserProxy struct {
	Server   string
	Username string
	Password string
}

const shadowDOMBootstrap = `(() => {
	const collect = (root, selector, result) => {
		if (!root || !root.querySelectorAll) return;
		for (const element of root.querySelectorAll(selector)) result.push(element);
		for (const element of root.querySelectorAll('*')) {
			if (element.shadowRoot) collect(element.shadowRoot, selector, result);
		}
	};
	window.__cinekoQueryAll = (selector, root = document) => {
		const result = [];
		collect(root, selector, result);
		return result;
	};
	window.__cinekoQuery = (selector, root = document) => window.__cinekoQueryAll(selector, root)[0] || null;
})();`

func DefaultBrowserConfig() BrowserConfig {
	return BrowserConfig{
		ProfileDir:     filepath.Join(".cineko", "chrome-profile"),
		ArtifactsDir:   filepath.Join(".cineko", "artifacts"),
		Headless:       true,
		BlockResources: true,
		TimeZone:       "Asia/Seoul",
	}
}

type Adapter struct {
	ctx               context.Context
	cancelContext     context.CancelFunc
	browserContext    playwright.BrowserContext
	page              playwright.Page
	identitySession   playwright.CDPSession
	stopPlaywright    func() error
	closeOnce         sync.Once
	lifecycleMu       sync.Mutex
	closeHooks        []func()
	closed            bool
	artifactsDir      string
	mu                sync.Mutex
	selectedRegion    string
	selectedTheater   string
	blockedRequests   atomic.Uint64
	continuedRequests atomic.Uint64
	blockResources    bool
	userAgent         browserUserAgent
	userAgentMetadata userAgentBootstrapIdentity
	webGLIdentity     webGLIdentity
}

type BrowserPool struct {
	config     BrowserConfig
	closeOnce  sync.Once
	mu         sync.Mutex
	closed     bool
	active     map[*Adapter]struct{}
	slot       chan struct{}
	playwright *playwright.Playwright
	openings   sync.WaitGroup
}

func NewBrowserPool(config BrowserConfig) (*BrowserPool, error) {
	config = normalizedBrowserConfig(config)
	if err := prepareBrowserDirectories(config); err != nil {
		return nil, err
	}
	pw, err := startPlaywright()
	if err != nil {
		return nil, fmt.Errorf("start Playwright: %w", err)
	}
	config.ChromePath, err = resolveBrowserExecutable(pw, config.ChromePath)
	if err != nil {
		_ = pw.Stop()
		return nil, err
	}
	capacity := config.Capacity
	if capacity < 1 {
		capacity = 1
	}
	pool := &BrowserPool{
		config:     config,
		active:     make(map[*Adapter]struct{}, capacity),
		slot:       make(chan struct{}, capacity),
		playwright: pw,
	}
	for range capacity {
		pool.slot <- struct{}{}
	}
	return pool, nil
}

func (pool *BrowserPool) NewAdapter(parent context.Context, config BrowserConfig) (*Adapter, error) {
	if parent == nil {
		return nil, errors.New("browser task context is required")
	}
	select {
	case <-parent.Done():
		return nil, parent.Err()
	case <-pool.slot:
	}
	releaseSlot := true
	defer func() {
		if releaseSlot {
			pool.slot <- struct{}{}
		}
	}()

	config = normalizedBrowserConfig(config)
	if config.ChromePath == "" {
		config.ChromePath = pool.config.ChromePath
	} else {
		var err error
		config.ChromePath, err = resolveBrowserExecutable(pool.playwright, config.ChromePath)
		if err != nil {
			return nil, err
		}
	}
	if config.ArtifactsDir == DefaultBrowserConfig().ArtifactsDir {
		config.ArtifactsDir = pool.config.ArtifactsDir
	}
	if err := prepareBrowserDirectories(config); err != nil {
		return nil, err
	}
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return nil, errors.New("browser pool is closed")
	}
	pool.openings.Add(1)
	pool.mu.Unlock()
	defer pool.openings.Done()

	// A logical automation task owns one Chrome process and its only page
	// target. Closing the adapter tears the process down, so no browser state,
	// handles, or renderer memory can accumulate across sequential tasks.
	adapter, err := newAdapter(parent, pool.playwright, config, nil)
	if err != nil {
		return nil, err
	}
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		adapter.Close()
		return nil, errors.New("browser pool is closed")
	}
	pool.active[adapter] = struct{}{}
	pool.mu.Unlock()
	adapter.AddCloseHook(func() { pool.releaseAdapter(adapter) })
	releaseSlot = false
	return adapter, nil
}

func (pool *BrowserPool) releaseAdapter(adapter *Adapter) {
	pool.mu.Lock()
	if _, exists := pool.active[adapter]; exists {
		delete(pool.active, adapter)
		pool.slot <- struct{}{}
	}
	pool.mu.Unlock()
}

func (pool *BrowserPool) Close() {
	pool.closeOnce.Do(func() {
		pool.mu.Lock()
		pool.closed = true
		adapters := make([]*Adapter, 0, len(pool.active))
		for adapter := range pool.active {
			adapters = append(adapters, adapter)
		}
		pool.mu.Unlock()
		pool.openings.Wait()
		for _, adapter := range adapters {
			adapter.Close()
		}
		if pool.playwright != nil {
			_ = pool.playwright.Stop()
		}
	})
}

func NewAdapter(parent context.Context, config BrowserConfig) (*Adapter, error) {
	if parent == nil {
		return nil, errors.New("browser task context is required")
	}
	config = normalizedBrowserConfig(config)
	if err := prepareBrowserDirectories(config); err != nil {
		return nil, err
	}
	pw, err := startPlaywright()
	if err != nil {
		return nil, fmt.Errorf("start Playwright: %w", err)
	}
	config.ChromePath, err = resolveBrowserExecutable(pw, config.ChromePath)
	if err != nil {
		_ = pw.Stop()
		return nil, err
	}
	adapter, err := newAdapter(parent, pw, config, pw.Stop)
	if err != nil {
		_ = pw.Stop()
		return nil, err
	}
	return adapter, nil
}

func newAdapter(
	parent context.Context,
	pw *playwright.Playwright,
	config BrowserConfig,
	stopPlaywright func() error,
) (*Adapter, error) {
	if pw == nil {
		return nil, errors.New("playwright runtime is required")
	}
	adapterContext, cancelContext := context.WithCancel(parent)
	persistedIdentity, err := loadSessionIdentity(config)
	if err != nil {
		cancelContext()
		return nil, err
	}
	var selectedUserAgent browserUserAgent
	if persistedIdentity != nil {
		selectedUserAgent = persistedIdentity.UserAgent
	} else {
		selectedUserAgent, err = selectBrowserUserAgent(config.ChromePath, config.UserAgentMode, nil)
		if err != nil {
			cancelContext()
			return nil, err
		}
	}
	locale := browserLocale(config, persistedIdentity)
	options := persistentContextOptions(config, locale)
	browserContext, err := pw.Chromium.LaunchPersistentContext(config.ProfileDir, options)
	if err != nil {
		cancelContext()
		return nil, fmt.Errorf("launch Chrome with Playwright: %w", err)
	}
	page, err := onlyBrowserPage(browserContext)
	if err != nil {
		_ = browserContext.Close()
		cancelContext()
		return nil, err
	}
	if persistedIdentity == nil {
		selectedUserAgent.Value, err = readNativeReducedUserAgent(page, selectedUserAgent.Major)
		if err != nil {
			_ = browserContext.Close()
			cancelContext()
			return nil, err
		}
	}
	identity, err := initializeBrowserIdentity(page, selectedUserAgent, persistedIdentity)
	if err != nil {
		_ = browserContext.Close()
		cancelContext()
		return nil, err
	}
	adapter := &Adapter{
		ctx: adapterContext, cancelContext: cancelContext, browserContext: browserContext, page: page,
		identitySession: identity.session,
		stopPlaywright:  stopPlaywright, artifactsDir: config.ArtifactsDir,
		userAgent:         selectedUserAgent,
		userAgentMetadata: identity.metadata, webGLIdentity: identity.webGL,
		blockResources: config.BlockResources,
	}
	if persistedIdentity == nil {
		if err := saveSessionIdentity(config, persistentBrowserIdentity{
			Version: sessionIdentityVersion, UserAgent: selectedUserAgent,
			Metadata: identity.metadata, Languages: identity.languages, WebGL: identity.webGL,
		}); err != nil {
			adapter.Close()
			return nil, err
		}
	}
	if err := adapter.installBrowserHooks(identity.scripts); err != nil {
		adapter.Close()
		return nil, err
	}
	go func() {
		<-adapterContext.Done()
		adapter.Close()
	}()
	return adapter, nil
}

func browserLocale(config BrowserConfig, persisted *persistentBrowserIdentity) string {
	if config.Locale != "" {
		return config.Locale
	}
	if persisted != nil {
		return persisted.Languages[0]
	}
	if config.UserAgentMode == UserAgentSession {
		return profilePrimaryLanguage(config.ProfileDir)
	}
	return ""
}

func onlyBrowserPage(browserContext playwright.BrowserContext) (playwright.Page, error) {
	pages := browserContext.Pages()
	if len(pages) == 0 {
		page, err := browserContext.NewPage()
		if err != nil {
			return nil, fmt.Errorf("create browser page: %w", err)
		}
		pages = []playwright.Page{page}
	}
	for _, extraPage := range pages[1:] {
		_ = extraPage.Close()
	}
	return pages[0], nil
}

type browserIdentitySetup struct {
	session   playwright.CDPSession
	webGL     webGLIdentity
	metadata  userAgentBootstrapIdentity
	languages []string
	scripts   []string
}

func initializeBrowserIdentity(
	page playwright.Page,
	userAgent browserUserAgent,
	persisted *persistentBrowserIdentity,
) (browserIdentitySetup, error) {
	var languages []string
	var webGL webGLIdentity
	var metadata userAgentBootstrapIdentity
	var err error
	if persisted == nil {
		languages, err = readNativeBrowserLanguages(page)
		if err != nil {
			return browserIdentitySetup{}, err
		}
		languages = languages[:1]
		webGL, err = readNativeWebGLIdentity(page)
		if err != nil {
			return browserIdentitySetup{}, err
		}
		metadata, err = readNativeUserAgentIdentity(page, userAgent)
		if err != nil {
			return browserIdentitySetup{}, err
		}
	} else {
		languages = append([]string(nil), persisted.Languages...)
		webGL = persisted.WebGL
		metadata = persisted.Metadata
	}
	userAgentScript, err := browserUserAgentBootstrap(metadata)
	if err != nil {
		return browserIdentitySetup{}, err
	}
	session, err := openBrowserIdentitySession(page, userAgent, metadata)
	if err != nil {
		return browserIdentitySetup{}, err
	}
	localizedStealth, err := stealthBootstrapForIdentity(languages, webGL)
	if err != nil {
		_ = session.Detach()
		return browserIdentitySetup{}, fmt.Errorf("configure browser stealth: %w", err)
	}
	return browserIdentitySetup{
		session:   session,
		webGL:     webGL,
		metadata:  metadata,
		languages: languages,
		scripts: []string{
			localizedStealth, chromeStealthBootstrap, userAgentScript, cinekoStealthOverrides, shadowDOMBootstrap,
		},
	}, nil
}

func (adapter *Adapter) installBrowserHooks(scripts []string) error {
	for _, script := range scripts {
		if err := adapter.browserContext.AddInitScript(
			playwright.Script{Content: playwright.String(script)},
		); err != nil {
			return fmt.Errorf("install browser init script: %w", err)
		}
	}
	if err := adapter.browserContext.Route("**/*", adapter.routeRequest); err != nil {
		return fmt.Errorf("install browser resource routing: %w", err)
	}
	adapter.browserContext.OnPage(func(page playwright.Page) {
		if page != adapter.page {
			_ = page.Close()
		}
	})
	for _, script := range scripts {
		if _, err := adapter.page.Evaluate(script); err != nil {
			return fmt.Errorf("initialize browser page: %w", err)
		}
	}
	return nil
}

func readNativeBrowserLanguages(page playwright.Page) ([]string, error) {
	value, err := page.Evaluate(`Array.from(navigator.languages || [navigator.language]).filter(Boolean)`)
	if err != nil {
		return nil, fmt.Errorf("read browser languages: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode browser languages: %w", err)
	}
	var languages []string
	if err := json.Unmarshal(encoded, &languages); err != nil {
		return nil, fmt.Errorf("decode browser languages: %w", err)
	}
	unique := make([]string, 0, len(languages))
	seen := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		language = strings.TrimSpace(language)
		if language == "" {
			continue
		}
		key := strings.ToLower(language)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, language)
	}
	if len(unique) == 0 {
		return nil, errors.New("browser returned no languages")
	}
	return unique, nil
}

func profilePrimaryLanguage(profileDir string) string {
	contents, err := os.ReadFile(filepath.Join(profileDir, "Default", "Preferences")) // #nosec G304 -- app profile path.
	if err != nil {
		return ""
	}
	var preferences struct {
		Intl struct {
			AcceptLanguages string `json:"accept_languages"`
		} `json:"intl"`
	}
	if json.Unmarshal(contents, &preferences) != nil {
		return ""
	}
	primary, _, _ := strings.Cut(preferences.Intl.AcceptLanguages, ",")
	return strings.TrimSpace(primary)
}

func normalizedBrowserConfig(config BrowserConfig) BrowserConfig {
	defaults := DefaultBrowserConfig()
	if config.ProfileDir == "" {
		config.ProfileDir = defaults.ProfileDir
	}
	if config.ArtifactsDir == "" {
		config.ArtifactsDir = defaults.ArtifactsDir
	}
	if config.UserAgentMode == "" {
		config.UserAgentMode = UserAgentSession
	}
	if config.TimeZone == "" {
		config.TimeZone = defaults.TimeZone
	}
	return config
}

func prepareBrowserDirectories(config BrowserConfig) error {
	for _, directory := range []string{config.ProfileDir, config.ArtifactsDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create browser directory: %w", err)
		}
	}
	return nil
}

func persistentContextOptions(
	config BrowserConfig,
	locale string,
) playwright.BrowserTypeLaunchPersistentContextOptions {
	position := "--window-position=80,80"
	if config.StartMinimized && !config.Headless {
		position = "--window-position=-32000,-32000"
	}
	options := playwright.BrowserTypeLaunchPersistentContextOptions{
		ExecutablePath:    playwright.String(config.ChromePath),
		Headless:          playwright.Bool(config.Headless),
		IgnoreDefaultArgs: []string{"--enable-automation"},
		Args: []string{
			"--disable-blink-features=AutomationControlled",
			"--disable-background-timer-throttling",
			"--disable-backgrounding-occluded-windows",
			"--disable-renderer-backgrounding",
			"--window-size=1440,1100",
			position,
		},
		TimezoneId:     playwright.String(config.TimeZone),
		ServiceWorkers: playwright.ServiceWorkerPolicyBlock,
		Screen:         &playwright.Size{Width: 1440, Height: 1100},
		Viewport:       &playwright.Size{Width: 1440, Height: 1100},
	}
	if config.RestoreSession {
		options.Args = append(options.Args, "--restore-last-session")
		options.IgnoreDefaultArgs = append(options.IgnoreDefaultArgs, "--no-startup-window")
	}
	if locale != "" {
		options.Locale = playwright.String(locale)
	}
	if config.Proxy != nil {
		options.Proxy = &playwright.Proxy{
			Server:   config.Proxy.Server,
			Username: optionalString(config.Proxy.Username),
			Password: optionalString(config.Proxy.Password),
		}
	}
	return options
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return playwright.String(value)
}

var thirdPartyBlocklist = []string{
	"google-analytics.com",
	"googletagmanager.com",
	"doubleclick.net",
	"googlesyndication.com",
	"adservice.google.",
	"connect.facebook.net",
	"facebook.com/tr",
	"analytics",
	"tracking",
	"/pixel",
	"pixel.",
}

func shouldBlockResource(requestURL, resourceType string) bool {
	lowerURL := strings.ToLower(requestURL)
	for _, pattern := range thirdPartyBlocklist {
		if strings.Contains(lowerURL, pattern) {
			return true
		}
	}
	switch strings.ToLower(resourceType) {
	case "document", "xhr", "fetch":
		return false
	case "script":
		parsed, err := url.Parse(requestURL)
		if err != nil {
			return true
		}
		host := strings.ToLower(parsed.Hostname())
		return host != "cgv.co.kr" && !strings.HasSuffix(host, ".cgv.co.kr")
	default:
		return true
	}
}

func (adapter *Adapter) routeRequest(route playwright.Route) {
	request := route.Request()
	if adapter.blockResources && shouldBlockResource(request.URL(), request.ResourceType()) {
		adapter.blockedRequests.Add(1)
		_ = route.Abort("blockedbyclient")
		return
	}
	adapter.continuedRequests.Add(1)
	headers, err := request.AllHeaders()
	if err != nil {
		_ = route.Continue()
		return
	}
	applyUserAgentHeaders(headers, adapter.userAgent, adapter.userAgentMetadata)
	_ = route.Continue(playwright.RouteContinueOptions{Headers: headers})
}

func (adapter *Adapter) Close() {
	adapter.closeOnce.Do(func() {
		adapter.cancelContext()
		if adapter.identitySession != nil {
			_ = adapter.identitySession.Detach()
		}
		if adapter.browserContext != nil {
			_ = adapter.browserContext.Close()
		}
		if adapter.stopPlaywright != nil {
			_ = adapter.stopPlaywright()
		}
		adapter.lifecycleMu.Lock()
		adapter.closed = true
		hooks := append([]func(){}, adapter.closeHooks...)
		adapter.closeHooks = nil
		adapter.lifecycleMu.Unlock()
		for _, hook := range hooks {
			hook()
		}
	})
}

// AddCloseHook registers resource cleanup after Chrome has stopped. If Close
// already won the race, the hook runs synchronously instead of being lost.
func (adapter *Adapter) AddCloseHook(hook func()) {
	if hook == nil {
		return
	}
	adapter.lifecycleMu.Lock()
	if !adapter.closed {
		adapter.closeHooks = append(adapter.closeHooks, hook)
		adapter.lifecycleMu.Unlock()
		return
	}
	adapter.lifecycleMu.Unlock()
	hook()
}

func (adapter *Adapter) Capture(label string) (string, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.captureUnlocked(label)
}

func (adapter *Adapter) captureUnlocked(label string) (string, error) {
	screenshot, err := adapter.page.Screenshot(playwright.PageScreenshotOptions{
		FullPage: playwright.Bool(true),
		Type:     playwright.ScreenshotTypePng,
	})
	if err != nil {
		return "", err
	}
	label = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			return character
		}
		return '-'
	}, label)
	path := filepath.Join(
		adapter.artifactsDir,
		fmt.Sprintf("%s-%s.png", time.Now().Format("20060102T150405.000"), label),
	)
	if err := os.WriteFile(path, screenshot, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (adapter *Adapter) navigate(url string) error {
	if err := adapter.ctx.Err(); err != nil {
		return err
	}
	if _, err := adapter.page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		return err
	}
	if err := adapter.page.Locator("body").WaitFor(playwright.LocatorWaitForOptions{
		State: playwright.WaitForSelectorStateAttached,
	}); err != nil {
		return err
	}
	return adapter.wait(700 * time.Millisecond)
}

func (adapter *Adapter) evaluate(expression string, output any) error {
	if err := adapter.ctx.Err(); err != nil {
		return err
	}
	value, err := adapter.page.Evaluate(expression)
	if err != nil || output == nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, output)
}

func (adapter *Adapter) wait(duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-adapter.ctx.Done():
		return adapter.ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (adapter *Adapter) clickButtonExact(label string) (bool, error) {
	expression := fmt.Sprintf(`(() => {
		const expected = %s;
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const element = window.__cinekoQueryAll('button')
			.find(button => !button.disabled && normalize(button.innerText || button.textContent) === expected);
		if (!element) return false;
		element.scrollIntoView({block: 'center'});
		element.click();
		return true;
	})()`, jsString(label))
	var clicked bool
	err := adapter.evaluate(expression, &clicked)
	return clicked, err
}

func (adapter *Adapter) clickButtonPrefix(prefix string) (bool, error) {
	expression := fmt.Sprintf(`(() => {
		const expected = %s;
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		const element = window.__cinekoQueryAll('button')
			.find(button => !button.disabled && normalize(button.innerText || button.textContent).startsWith(expected));
		if (!element) return false;
		element.scrollIntoView({block: 'center'});
		element.click();
		return true;
	})()`, jsString(prefix))
	var clicked bool
	err := adapter.evaluate(expression, &clicked)
	return clicked, err
}

func (adapter *Adapter) buttonExists(label string) (bool, error) {
	expression := fmt.Sprintf(`(() => {
		const expected = %s;
		const normalize = value => (value || '').replace(/\s+/g, ' ').trim();
		return window.__cinekoQueryAll('button')
			.some(button => normalize(button.innerText || button.textContent) === expected);
	})()`, jsString(label))
	var exists bool
	err := adapter.evaluate(expression, &exists)
	return exists, err
}

func jsString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
