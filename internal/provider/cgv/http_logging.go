package cgv

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cineko-org/probe/v2/internal/telemetry"
	"github.com/cineko-org/probe/v2/networkcapture/playwrightcapture"
	"github.com/mxschmitt/playwright-go"
)

func (adapter *Adapter) logProviderRequest(request playwright.Request) {
	adapter.captureNetworkExchange(request, false)
	adapter.logProviderRequestCompletion(request, false)
}

func (adapter *Adapter) logProviderRequestFailed(request playwright.Request) {
	adapter.observeRateLimitFailure(request)
	adapter.captureNetworkExchange(request, true)
	adapter.logProviderRequestCompletion(request, true)
}

func (adapter *Adapter) observeRateLimitFailure(request playwright.Request) {
	if adapter == nil || adapter.rateLimit == nil || request == nil {
		return
	}
	decision, observed := adapter.rateLimit.ObserveFailure(rateLimitKey(request.URL()))
	if !observed || adapter.logger == nil {
		return
	}
	adapter.logger.WarnContext(adapter.ctx, "Provider rate limit half-open request failed",
		"event", "browser.network.rate_limit.half_open_failed", "scenario", "schedule_collection",
		"operation", "probe_provider_rate_limit", "outcome", "blocked",
		"request_url", request.URL(), "retry_at", decision.BlockedUntil,
		"retry_after_ms", decision.Delay.Milliseconds(), "rate_limit_failures", decision.Failures)
}

func (adapter *Adapter) captureNetworkExchange(request playwright.Request, failed bool) {
	if adapter == nil || adapter.networkCapture == nil || request == nil {
		return
	}
	if !playwrightcapture.ShouldCapturePlaywrightRequest(adapter.networkCapture, request, failed) {
		return
	}
	if !adapter.beginNetworkCapture() {
		return
	}
	defer adapter.networkCaptureWG.Done()
	record := playwrightcapture.PlaywrightRecord(request, failed)
	record.Service = "probe"
	record.Scenario = "cgv_browser"
	record.CorrelationID = adapter.browserRequestID(request)
	if _, err := adapter.networkCapture.Save(context.WithoutCancel(adapter.ctx), record); err != nil && adapter.logger != nil {
		adapter.logger.ErrorContext(adapter.ctx, "Browser network exchange capture failed",
			"event", "browser.network.capture.failed", "request_id", record.CorrelationID,
			"method", record.Request.Method, "request_url", record.Request.URL, "error", err)
	}
}

func (adapter *Adapter) beginNetworkCapture() bool {
	adapter.networkCaptureMu.Lock()
	defer adapter.networkCaptureMu.Unlock()
	if adapter.networkCaptureClosing {
		return false
	}
	adapter.networkCaptureWG.Add(1)
	return true
}

func (adapter *Adapter) stopNetworkCapture() {
	adapter.networkCaptureMu.Lock()
	adapter.networkCaptureClosing = true
	adapter.networkCaptureMu.Unlock()
	adapter.networkCaptureWG.Wait()
}

func (adapter *Adapter) observeRateLimitResponse(response playwright.Response) {
	if adapter == nil || adapter.rateLimit == nil || response == nil {
		return
	}
	key := rateLimitKey(response.URL())
	if response.Status() != 429 {
		if adapter.rateLimit.ObserveSuccess(key) && adapter.logger != nil {
			adapter.logger.InfoContext(adapter.ctx, "Provider rate limit circuit closed",
				"event", "browser.network.rate_limit.closed", "request_url", response.URL(),
				"status", response.Status(), "outcome", "recovered")
		}
		return
	}
	headers, _ := response.HeadersArray()
	decision := adapter.rateLimit.Observe429(key, playwrightcapture.PlaywrightHeaders(headers))
	if adapter.logger != nil {
		adapter.logger.ErrorContext(adapter.ctx, "Provider rate limit circuit opened",
			"event", "browser.network.rate_limit.opened", "scenario", "schedule_collection",
			"operation", "observe_provider_response", "outcome", "blocked",
			"request_url", response.URL(), "status", response.Status(),
			"retry_at", decision.BlockedUntil, "retry_after_ms", decision.Delay.Milliseconds(),
			"rate_limit_source", decision.Source, "rate_limit_failures", decision.Failures,
			"error", ErrProviderThrottled)
	}
}

func rateLimitKey(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return parsed.Hostname()
}

func (adapter *Adapter) providerRateLimitError(rawURL string) error {
	if adapter == nil || adapter.rateLimit == nil {
		return nil
	}
	blocked, decision := adapter.rateLimit.Blocked(rateLimitKey(rawURL))
	if !blocked {
		return nil
	}
	return fmt.Errorf("%w: retry at %s (%s)", ErrProviderThrottled,
		decision.BlockedUntil.Format(time.RFC3339Nano), decision.Source)
}

func (adapter *Adapter) logProviderRequestCompletion(request playwright.Request, failed bool) {
	if adapter == nil || adapter.logger == nil || request == nil {
		return
	}
	requestID := adapter.browserRequestID(request)
	path, host := browserRequestLocation(request.URL())
	requestBytes, responseBytes := browserRequestSizes(request)
	status, responseErr := browserRequestStatus(request, failed)
	attrs := []any{
		"service", "probe",
		"event", "browser.network.request.completed",
		"transport", "chromium",
		"request_id", requestID,
		"method", request.Method(),
		"route", path,
		"path", path,
		"host", host,
		"resource_type", request.ResourceType(),
		"duration_ms", browserRequestDuration(request),
		"status", status,
		"request_bytes", requestBytes,
		"response_bytes", responseBytes,
	}
	rawErr := browserRequestError(request, failed, status, responseErr)
	adapter.writeBrowserRequestLog(attrs, rawErr)
}

func (adapter *Adapter) browserRequestID(request playwright.Request) string {
	if value, err := request.HeaderValue(telemetry.RequestIDHeader); err == nil && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if requestID := telemetry.RequestID(adapter.ctx); requestID != "" {
		return requestID
	}
	return telemetry.NewRequestID()
}

func browserRequestLocation(rawURL string) (string, string) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "/", ""
	}
	path := parsedURL.Path
	if path == "" {
		path = "/"
	}
	return path, parsedURL.Hostname()
}

func browserRequestSizes(request playwright.Request) (int, int) {
	sizes, err := request.Sizes()
	if err != nil || sizes == nil {
		return 0, 0
	}
	return sizes.RequestBodySize, sizes.ResponseBodySize
}

func browserRequestStatus(request playwright.Request, failed bool) (int, error) {
	if response, err := request.ExistingResponse(); err == nil && response != nil {
		return response.Status(), nil
	}
	if failed {
		return 0, nil
	}
	response, err := request.Response()
	if err != nil {
		return 0, err
	}
	if response == nil {
		return 0, nil
	}
	return response.Status(), nil
}

func browserRequestError(request playwright.Request, failed bool, status int, responseErr error) error {
	if requestErr := request.Failure(); requestErr != nil {
		return requestErr
	}
	if responseErr != nil {
		return responseErr
	}
	if failed {
		return errors.New("browser request failed")
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

func (adapter *Adapter) writeBrowserRequestLog(attrs []any, rawErr error) {
	if outcome := expectedBrowserRequestOutcome(rawErr); outcome != "" {
		attrs = append(attrs, "outcome", outcome)
		if outcome == "blocked" {
			attrs = append(attrs, "block_reason", "intentional_resource_filter")
		}
		adapter.logger.Debug("Browser network request completed", attrs...)
		return
	}
	if rawErr != nil {
		attrs = append(attrs, "outcome", "failed", "error", rawErr)
		adapter.logger.Error("Browser network request completed", attrs...)
		return
	}
	attrs = append(attrs, "outcome", "succeeded")
	adapter.logger.Debug("Browser network request completed", attrs...)
}

func expectedBrowserRequestOutcome(err error) string {
	if err == nil {
		return ""
	}
	reason := strings.ToUpper(err.Error())
	switch {
	case strings.Contains(reason, "ERR_BLOCKED_BY_CLIENT"), strings.Contains(reason, "BLOCKEDBYCLIENT"):
		return "blocked"
	case strings.Contains(reason, "ERR_ABORTED"):
		return "canceled"
	default:
		return ""
	}
}

func browserRequestDuration(request playwright.Request) int64 {
	if request == nil {
		return 0
	}
	timing := request.Timing()
	if timing == nil || timing.ResponseEnd < 0 {
		return 0
	}
	return int64(timing.ResponseEnd)
}
