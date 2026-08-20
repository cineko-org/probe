package egress

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSessionTTL = 30 * time.Minute
	minimumSessionTTL = 5 * time.Minute
	maximumSessionTTL = 24 * time.Hour
	maximumTokenBytes = 16 << 10
	maximumProxyBytes = 64 << 10
)

var (
	// ErrNoProxyCapacity indicates that a managed proxy has no available slot.
	ErrNoProxyCapacity = errors.New("no egress proxy is currently available")
	// ErrLeaseRenewal indicates that a managed proxy lease could not be renewed.
	ErrLeaseRenewal = errors.New("egress proxy lease renewal failed")
	// ErrUnknownPolicy indicates that an assignment references an unsupported policy.
	ErrUnknownPolicy = errors.New("unknown egress policy")
)

// Purpose identifies the browser operation receiving an egress lease.
type Purpose string

const (
	// PurposeSession is authenticated user-facing browser work.
	PurposeSession Purpose = "session"
	// PurposeScan is anonymous provider observation.
	PurposeScan Purpose = "scan"

	// PolicyScanDefault is the Central assignment policy for a Probe scan. It
	// prefers the locally configured static or Soxy proxy and otherwise uses
	// direct egress. Managed deployments that require a proxy enforce that
	// separately during startup with CINEKO_REQUIRE_PROXY=true.
	PolicyScanDefault = "scan_default"
)

// Proxy describes one outbound HTTP proxy endpoint.
type Proxy struct {
	Server   string
	Username string
	Password string
}

type secretFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

// Config contains the local egress inventory and lease timing policy.
type Config struct {
	SoxyURL    string
	SoxyToken  string
	SessionTTL time.Duration
	Proxies    []Proxy
	// ScanProxies is retained for environment compatibility. New callers use
	// Proxies, which applies one stable selection to either logical purpose.
	ScanProxies      []Proxy
	HTTPClient       *http.Client
	Random           io.Reader
	RenewInterval    time.Duration
	MaxRenewFailures int
	Probe            func(context.Context, Proxy) error
}

// Manager resolves egress policy and owns active proxy leases.
type Manager struct {
	client            *soxyClient
	sessionTTL        time.Duration
	proxies           []Proxy
	legacyScanProxies []Proxy
	random            io.Reader
	renewInterval     time.Duration
	maxRenewFailures  int
}

// NewFromEnvironment loads the supported secret-file configuration and creates an egress manager.
func NewFromEnvironment() (*Manager, error) {
	config, err := ConfigFromEnvironment()
	if err != nil {
		return nil, err
	}
	return New(config)
}

// ConfigFromEnvironment reads the optional CLI and managed-deployment egress settings.
func ConfigFromEnvironment() (Config, error) {
	return configFromLookup(os.LookupEnv)
}

// New validates egress configuration and creates a manager.
func New(config Config) (*Manager, error) {
	config.SoxyURL = strings.TrimSpace(config.SoxyURL)
	config.SoxyToken = strings.TrimSpace(config.SoxyToken)
	if (config.SoxyURL == "") != (config.SoxyToken == "") {
		return nil, errors.New("configure Soxy URL and API token together")
	}
	proxies, legacyScanProxies, err := configuredProxyPools(config)
	if err != nil {
		return nil, err
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = defaultSessionTTL
	}
	if config.SessionTTL < minimumSessionTTL || config.SessionTTL > maximumSessionTTL {
		return nil, fmt.Errorf("soxy session TTL must be between %s and %s", minimumSessionTTL, maximumSessionTTL)
	}
	if config.Random == nil {
		config.Random = cryptorand.Reader
	}
	if config.MaxRenewFailures == 0 {
		config.MaxRenewFailures = 3
	}
	if config.MaxRenewFailures < 1 {
		return nil, errors.New("maximum lease renewal failures must be positive")
	}
	if config.RenewInterval == 0 {
		config.RenewInterval = config.SessionTTL / 2
	}
	if config.RenewInterval < 0 {
		return nil, errors.New("lease renewal interval cannot be negative")
	}

	manager := &Manager{
		sessionTTL:        config.SessionTTL,
		proxies:           proxies,
		legacyScanProxies: legacyScanProxies,
		random:            config.Random,
		renewInterval:     config.RenewInterval,
		maxRenewFailures:  config.MaxRenewFailures,
	}
	if config.SoxyURL != "" {
		client, err := newSoxyClient(config.SoxyURL, config.SoxyToken, config.HTTPClient)
		if err != nil {
			return nil, err
		}
		manager.client = client
	}
	return manager, nil
}

func configuredProxyPools(config Config) ([]Proxy, []Proxy, error) {
	if config.SoxyURL != "" && len(config.Proxies)+len(config.ScanProxies) > 0 {
		return nil, nil, errors.New("configure either Soxy or standard proxies, not both")
	}
	proxies, err := normalizeProxies(config.Proxies)
	if err != nil {
		return nil, nil, fmt.Errorf("configure standard proxies: %w", err)
	}
	legacyScanProxies, err := normalizeProxies(config.ScanProxies)
	if err != nil {
		return nil, nil, fmt.Errorf("configure scan proxies: %w", err)
	}
	return proxies, legacyScanProxies, nil
}

func normalizeProxies(values []Proxy) ([]Proxy, error) {
	result := make([]Proxy, 0, len(values))
	for _, value := range values {
		parsed, err := ParseProxy(value.Server)
		if err != nil {
			return nil, err
		}
		if value.Username != "" {
			parsed.Username = value.Username
			parsed.Password = value.Password
		}
		result = append(result, parsed)
	}
	return result, nil
}

func configFromLookup(lookup func(string) (string, bool)) (Config, error) {
	return configFromSources(lookup, readSecretFile)
}

func configFromSources(
	lookup func(string) (string, bool),
	readSecret func(string, int64) (string, error),
) (Config, error) {
	config := Config{SessionTTL: defaultSessionTTL}
	if value, exists := lookup("CINEKO_SOXY_URL"); exists && strings.TrimSpace(value) != "" {
		config.SoxyURL = strings.TrimSpace(value)
	}
	if err := validateSecretFileOnlyEnvironment(lookup); err != nil {
		return Config{}, err
	}
	token, err := secretFromLookup(lookup, readSecret, "CINEKO_SOXY_API_TOKEN_FILE", maximumTokenBytes)
	if err != nil {
		return Config{}, err
	}
	config.SoxyToken = token
	config.SessionTTL, err = sessionTTLFromLookup(lookup)
	if err != nil {
		return Config{}, err
	}
	proxySecret, err := secretFromLookup(lookup, readSecret, "CINEKO_SCAN_PROXIES_FILE", maximumProxyBytes)
	if err != nil {
		return Config{}, err
	}
	config.ScanProxies, err = parseProxySecret(proxySecret)
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

func validateSecretFileOnlyEnvironment(lookup func(string) (string, bool)) error {
	if value, exists := lookup("CINEKO_SOXY_API_TOKEN"); exists && strings.TrimSpace(value) != "" {
		return errors.New("CINEKO_SOXY_API_TOKEN is not supported; use CINEKO_SOXY_API_TOKEN_FILE")
	}
	if value, exists := lookup("CINEKO_SCAN_PROXIES"); exists && strings.TrimSpace(value) != "" {
		return errors.New("CINEKO_SCAN_PROXIES is not supported; use CINEKO_SCAN_PROXIES_FILE")
	}
	return nil
}

func sessionTTLFromLookup(lookup func(string) (string, bool)) (time.Duration, error) {
	value, exists := lookup("CINEKO_SOXY_SESSION_TTL")
	if !exists || strings.TrimSpace(value) == "" {
		return defaultSessionTTL, nil
	}
	ttl, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("parse CINEKO_SOXY_SESSION_TTL: %w", err)
	}
	return ttl, nil
}

func parseProxySecret(secret string) ([]Proxy, error) {
	rawProxies := strings.FieldsFunc(secret, func(character rune) bool {
		return character == ',' || character == '\n' || character == '\r'
	})
	proxies := make([]Proxy, 0, len(rawProxies))
	for index, rawProxy := range rawProxies {
		proxy, parseErr := ParseProxy(strings.TrimSpace(rawProxy))
		if parseErr != nil {
			return nil, fmt.Errorf("parse CINEKO_SCAN_PROXIES_FILE entry %d: invalid proxy configuration", index+1)
		}
		proxies = append(proxies, proxy)
	}
	return proxies, nil
}

func secretFromLookup(
	lookup func(string) (string, bool),
	readSecret func(string, int64) (string, error),
	name string,
	limit int64,
) (string, error) {
	value, exists := lookup(name)
	path := strings.TrimSpace(value)
	if !exists || path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute secret file path", name)
	}
	value, err := readSecret(path, limit)
	if err != nil {
		return "", fmt.Errorf("read %s: secret file is unavailable or invalid", name)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("read %s: secret file is empty", name)
	}
	return value, nil
}

func readSecretFile(path string, limit int64) (string, error) {
	return readSecretFileWith(path, limit, os.Lstat, func(path string) (secretFile, error) {
		return os.Open(path) // #nosec G304 -- absolute operator-configured Docker secret path.
	})
}

func readSecretFileWith(
	path string,
	limit int64,
	lstat func(string) (os.FileInfo, error),
	open func(string) (secretFile, error),
) (string, error) {
	linkInfo, err := lstat(path)
	if err != nil {
		return "", err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() {
		return "", errors.New("secret path is not a regular file")
	}
	file, err := open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(linkInfo, info) || !info.Mode().IsRegular() || info.Mode().Perm()&^0o640 != 0 {
		return "", errors.New("secret file permissions are broader than 0640")
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(contents)) > limit {
		return "", errors.New("secret file exceeds size limit")
	}
	return string(contents), nil
}

// ParseProxy validates and normalizes one HTTP, HTTPS, or SOCKS5 proxy URL.
func ParseProxy(rawURL string) (Proxy, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Proxy{}, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5" {
		return Proxy{}, fmt.Errorf("proxy scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Hostname() == "" || parsed.Port() == "" {
		return Proxy{}, errors.New("proxy URL must include a host and port")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return Proxy{}, errors.New("proxy URL contains an invalid port")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Proxy{}, errors.New("proxy URL cannot contain a path, query, or fragment")
	}
	proxy := Proxy{Server: parsed.Scheme + "://" + parsed.Host}
	if parsed.User != nil {
		proxy.Username = parsed.User.Username()
		proxy.Password, _ = parsed.User.Password()
	}
	return proxy, nil
}

// Acquire obtains a lease for a purpose using the manager's default policy.
func (manager *Manager) Acquire(parent context.Context, purpose Purpose) (*Lease, error) {
	return manager.acquire(parent, purpose)
}

// AcquireForPolicy resolves an assignment egress policy against this Probe's
// locally configured proxy inventory. Central chooses the policy, while proxy
// addresses and credentials remain local to Probe.
func (manager *Manager) AcquireForPolicy(
	parent context.Context,
	purpose Purpose,
	policyID string,
) (*Lease, error) {
	if purpose != PurposeScan {
		return nil, fmt.Errorf("%w %q for purpose %q", ErrUnknownPolicy, policyID, purpose)
	}
	if strings.TrimSpace(policyID) != PolicyScanDefault {
		return nil, fmt.Errorf("%w %q", ErrUnknownPolicy, policyID)
	}
	return manager.acquire(parent, purpose)
}

func (manager *Manager) acquire(parent context.Context, purpose Purpose) (*Lease, error) {
	if parent == nil {
		return nil, errors.New("proxy lease context is required")
	}
	if purpose != PurposeSession && purpose != PurposeScan {
		return nil, fmt.Errorf("unknown proxy purpose %q", purpose)
	}
	if len(manager.proxies) > 0 {
		index, err := randomIndex(manager.random, len(manager.proxies))
		if err != nil {
			return nil, fmt.Errorf("select proxy: %w", err)
		}
		return newLease(parent, manager.proxies[index], nil, 0, 0), nil
	}
	if purpose == PurposeScan && len(manager.legacyScanProxies) > 0 {
		index, err := randomIndex(manager.random, len(manager.legacyScanProxies))
		if err != nil {
			return nil, fmt.Errorf("select scan proxy: %w", err)
		}
		return newLease(parent, manager.legacyScanProxies[index], nil, 0, 0), nil
	}
	if manager.client == nil {
		return newLease(parent, Proxy{}, nil, 0, 0), nil
	}

	var session soxySession
	var err error
	if purpose == PurposeScan {
		session, err = manager.acquireRandomSoxySession(parent)
	} else {
		session, err = manager.client.createSession(parent, manager.sessionTTL, "")
	}
	if err != nil {
		return nil, err
	}
	proxy, err := proxyFromSoxy(session.Proxy)
	if err != nil {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = manager.client.releaseSession(cleanupContext, session.ID)
		cancel()
		return nil, fmt.Errorf("invalid proxy returned by Soxy: %w", err)
	}
	release := func(ctx context.Context) error {
		return manager.client.releaseSession(ctx, session.ID)
	}
	extend := func(ctx context.Context) error {
		return manager.client.extendSession(ctx, session.ID, manager.sessionTTL/2)
	}
	return newLease(parent, proxy, release, manager.renewInterval, manager.maxRenewFailures, extend), nil
}

func (manager *Manager) acquireRandomSoxySession(ctx context.Context) (soxySession, error) {
	slots, err := manager.client.availableSlots(ctx)
	if err != nil {
		return soxySession{}, err
	}
	for len(slots) > 0 {
		index, selectErr := randomIndex(manager.random, len(slots))
		if selectErr != nil {
			return soxySession{}, fmt.Errorf("select Soxy slot: %w", selectErr)
		}
		slot := slots[index]
		slots[index] = slots[len(slots)-1]
		slots = slots[:len(slots)-1]
		session, createErr := manager.client.createSession(ctx, manager.sessionTTL, slot.ID)
		if createErr == nil {
			return session, nil
		}
		var apiErr *soxyAPIError
		if !errors.As(createErr, &apiErr) || apiErr.Status != http.StatusConflict && apiErr.Status != http.StatusNotFound {
			return soxySession{}, createErr
		}
	}
	return soxySession{}, ErrNoProxyCapacity
}

func proxyFromSoxy(proxy soxyProxy) (Proxy, error) {
	if proxy.Host == "" || proxy.Port < 1 || proxy.Port > 65535 {
		return Proxy{}, errors.New("proxy host or port is invalid")
	}
	return ParseProxy((&url.URL{
		Scheme: proxy.Scheme,
		Host:   net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port)),
		User:   proxyUser(proxy.Username, proxy.Password),
	}).String())
}

func proxyUser(username, password string) *url.Userinfo {
	if username == "" {
		return nil
	}
	if password == "" {
		return url.User(username)
	}
	return url.UserPassword(username, password)
}

func randomIndex(random io.Reader, size int) (int, error) {
	if size < 1 {
		return 0, errors.New("cannot choose from an empty proxy set")
	}
	value, err := cryptorand.Int(random, big.NewInt(int64(size)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

type releaseFunc func(context.Context) error
type extendFunc func(context.Context) error

// Lease represents one bounded outbound-network reservation.
type Lease struct {
	proxy     Proxy
	ctx       context.Context
	cancel    context.CancelCauseFunc
	release   releaseFunc
	extend    extendFunc
	interval  time.Duration
	maxErrors int
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newLease(
	parent context.Context,
	proxy Proxy,
	release releaseFunc,
	interval time.Duration,
	maxErrors int,
	extenders ...extendFunc,
) *Lease {
	ctx, cancel := context.WithCancelCause(parent)
	lease := &Lease{proxy: proxy, ctx: ctx, cancel: cancel, release: release, interval: interval, maxErrors: maxErrors}
	if len(extenders) > 0 && extenders[0] != nil && interval > 0 {
		lease.extend = extenders[0]
		lease.done = make(chan struct{})
		go lease.keepAlive()
	}
	return lease
}

// Context is canceled when the lease expires or is closed.
func (lease *Lease) Context() context.Context { return lease.ctx }

// Proxy returns the selected endpoint, if this lease uses one.
func (lease *Lease) Proxy() *Proxy {
	if lease.proxy.Server == "" {
		return nil
	}
	proxy := lease.proxy
	return &proxy
}

// Close releases the lease and stops its renewal loop.
func (lease *Lease) Close() error {
	lease.closeOnce.Do(func() {
		lease.cancel(nil)
		if lease.done != nil {
			<-lease.done
		}
		if lease.release != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			lease.closeErr = lease.release(ctx)
			cancel()
		}
	})
	return lease.closeErr
}

func (lease *Lease) keepAlive() {
	defer close(lease.done)
	timer := time.NewTimer(lease.interval)
	defer timer.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-lease.ctx.Done():
			return
		case <-timer.C:
			if err := lease.extend(lease.ctx); err != nil {
				consecutiveFailures++
				if consecutiveFailures >= lease.maxErrors {
					lease.cancel(fmt.Errorf("%w: %w", ErrLeaseRenewal, err))
					return
				}
				timer.Reset(renewalRetryDelay(lease.interval))
			} else {
				consecutiveFailures = 0
				timer.Reset(lease.interval)
			}
		}
	}
}

func renewalRetryDelay(interval time.Duration) time.Duration {
	delay := interval / 10
	if delay < time.Millisecond {
		return time.Millisecond
	}
	if delay > 5*time.Second {
		return 5 * time.Second
	}
	return delay
}
