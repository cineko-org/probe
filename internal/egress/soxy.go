package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cineko-org/probe/v2/internal/telemetry"
)

type soxyClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type lookupIPAddr func(context.Context, string) ([]net.IPAddr, error)

type soxySlot struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CurrentIP string `json:"current_ip"`
}

type soxyProxy struct {
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type soxySession struct {
	ID     string    `json:"id"`
	Status string    `json:"status"`
	Ready  bool      `json:"ready"`
	Proxy  soxyProxy `json:"proxy"`
}

type soxyAPIError struct {
	Status int
	Code   string
}

func (err *soxyAPIError) Error() string {
	if err.Code != "" {
		return fmt.Sprintf("Soxy returned HTTP %d (%s)", err.Status, err.Code)
	}
	return fmt.Sprintf("Soxy returned HTTP %d", err.Status)
}

func newSoxyClient(rawURL, token string, httpClient *http.Client, loggers ...*slog.Logger) (*soxyClient, error) {
	return newSoxyClientWithResolver(rawURL, token, httpClient, net.DefaultResolver.LookupIPAddr, loggers...)
}

func newSoxyClientWithResolver(
	rawURL, token string,
	httpClient *http.Client,
	lookup lookupIPAddr,
	loggers ...*slog.Logger,
) (*soxyClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse Soxy URL: %w", err)
	}
	privateHTTPOrigin, err := validateSoxyOrigin(parsed, lookup)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		defaultTransport := http.DefaultTransport.(*http.Transport) //nolint:errcheck,forcetypeassert // standard library invariant.
		httpClient = defaultSoxyHTTPClient(privateHTTPOrigin, lookup, defaultTransport)
	}
	if len(loggers) > 0 && loggers[0] != nil {
		httpClient = telemetry.HTTPClient(loggers[0], httpClient)
	}
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &soxyClient{
		baseURL: strings.TrimRight(parsed.String(), "/"), token: strings.TrimSpace(token), httpClient: &client,
	}, nil
}

func validateSoxyOrigin(parsed *url.URL, lookup lookupIPAddr) (bool, error) {
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return false, errors.New("soxy URL must be an HTTP or HTTPS origin")
	}
	privateHTTPOrigin := parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) && !isPrivateIPHost(parsed.Hostname())
	if privateHTTPOrigin {
		validationContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := validatePrivateNetworkHost(validationContext, parsed.Hostname(), lookup)
		cancel()
		if err != nil {
			return false, errors.New("soxy URL must use HTTPS unless its host resolves only to private IP addresses")
		}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return false, errors.New("soxy URL must not contain credentials, a path, query, or fragment")
	}
	return privateHTTPOrigin, nil
}

func defaultSoxyHTTPClient(
	privateHTTPOrigin bool,
	lookup lookupIPAddr,
	baseTransport *http.Transport,
) *http.Client {
	transport := baseTransport.Clone()
	if privateHTTPOrigin {
		transport.DialContext = privateNetworkDialContext(lookup)
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}
}

func validatePrivateNetworkHost(ctx context.Context, host string, lookup lookupIPAddr) error {
	if lookup == nil {
		return errors.New("private network resolver is unavailable")
	}
	addresses, err := lookup(ctx, strings.TrimSpace(host))
	if err != nil || len(addresses) == 0 {
		return errors.New("private network host cannot be resolved")
	}
	for _, address := range addresses {
		if address.IP == nil || !address.IP.IsPrivate() && !address.IP.IsLoopback() {
			return errors.New("private network host resolved outside the private network")
		}
	}
	return nil
}

func privateNetworkDialContext(lookup lookupIPAddr) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse private network address: %w", err)
		}
		addresses, err := lookup(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("resolve private network address")
		}
		for _, resolved := range addresses {
			if resolved.IP == nil || !resolved.IP.IsPrivate() && !resolved.IP.IsLoopback() {
				return nil, errors.New("private network address resolved outside the private network")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
}

func isPrivateIPHost(host string) bool {
	address := net.ParseIP(strings.TrimSpace(host))
	return address != nil && address.IsPrivate()
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (client *soxyClient) availableSlots(ctx context.Context) ([]soxySlot, error) {
	var response struct {
		Slots []soxySlot `json:"slots"`
	}
	if err := client.request(ctx, http.MethodGet, "/v1/slots", nil, http.StatusOK, &response); err != nil {
		return nil, err
	}
	available := make([]soxySlot, 0, len(response.Slots))
	for _, slot := range response.Slots {
		if slot.ID != "" && slot.Status == "available" && slot.CurrentIP != "" {
			available = append(available, slot)
		}
	}
	if len(available) == 0 {
		return nil, ErrNoProxyCapacity
	}
	return available, nil
}

func (client *soxyClient) createSession(ctx context.Context, ttl time.Duration, slotID string) (soxySession, error) {
	payload := struct {
		TTLSeconds int64  `json:"ttl_seconds"`
		SlotID     string `json:"slot_id,omitempty"`
	}{TTLSeconds: int64(ttl / time.Second), SlotID: slotID}
	var session soxySession
	if err := client.request(ctx, http.MethodPost, "/v1/sessions", payload, http.StatusCreated, &session); err != nil {
		return soxySession{}, err
	}
	if session.ID == "" || session.Status != "active" || !session.Ready {
		if session.ID != "" {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = client.releaseSession(cleanupContext, session.ID)
			cancel()
		}
		return soxySession{}, errors.New("soxy returned a session that is not ready")
	}
	return session, nil
}

func (client *soxyClient) extendSession(ctx context.Context, sessionID string, extension time.Duration) error {
	payload := struct {
		ExtendBySeconds int64 `json:"extend_by_seconds"`
	}{ExtendBySeconds: int64(extension / time.Second)}
	return client.request(
		ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/extend", payload, http.StatusOK, nil,
	)
}

func (client *soxyClient) releaseSession(ctx context.Context, sessionID string) error {
	err := client.request(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID), nil, http.StatusNoContent, nil)
	var apiErr *soxyAPIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (client *soxyClient) reportProviderFailure(
	ctx context.Context,
	sessionID string,
	provider string,
	signal string,
	idempotencyKey string,
) error {
	payload := struct {
		Provider string `json:"provider"`
		Signal   string `json:"signal"`
	}{Provider: provider, Signal: signal}
	return client.requestWithHeaders(
		ctx,
		http.MethodPost,
		"/v1/sessions/"+url.PathEscape(sessionID)+"/provider-failures",
		payload,
		http.StatusAccepted,
		nil,
		map[string]string{"Idempotency-Key": idempotencyKey},
	)
}

func (client *soxyClient) request(
	ctx context.Context,
	method, path string,
	payload any,
	wantStatus int,
	destination any,
) error {
	return client.requestWithHeaders(ctx, method, path, payload, wantStatus, destination, nil)
}

func (client *soxyClient) requestWithHeaders(
	ctx context.Context,
	method, path string,
	payload any,
	wantStatus int,
	destination any,
	headers map[string]string,
) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode Soxy request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create Soxy request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call Soxy: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != wantStatus {
		limited, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(limited, &envelope)
		return &soxyAPIError{Status: response.StatusCode, Code: envelope.Error.Code}
	}
	if destination == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode Soxy response: %w", err)
	}
	return nil
}
