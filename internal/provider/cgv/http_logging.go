package cgv

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cineko-org/probe/v2/internal/telemetry"
	"github.com/mxschmitt/playwright-go"
)

func (adapter *Adapter) logProviderRequest(request playwright.Request) {
	adapter.logProviderRequestCompletion(request, false)
}

func (adapter *Adapter) logProviderRequestFailed(request playwright.Request) {
	adapter.logProviderRequestCompletion(request, true)
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
		adapter.logger.Info("Browser network request completed", attrs...)
		return
	}
	if rawErr != nil {
		attrs = append(attrs, "outcome", "failed", "error", rawErr)
		adapter.logger.Error("Browser network request completed", attrs...)
		return
	}
	attrs = append(attrs, "outcome", "succeeded")
	adapter.logger.Info("Browser network request completed", attrs...)
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
