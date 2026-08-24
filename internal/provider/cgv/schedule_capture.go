package cgv

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
)

type capturedProviderResponse struct {
	path       string
	requestURL string
	status     int
	body       []byte
	err        error
}

func (adapter *Adapter) captureProviderResponse(response playwright.Response) {
	if response == nil {
		return
	}
	path, ok := providerResponsePath(response.URL())
	if !ok {
		return
	}
	captured := capturedProviderResponse{path: path, requestURL: response.URL(), status: response.Status()}
	if captured.status < 200 || captured.status > 299 {
		captured.err = providerHTTPError(captured.status)
	} else {
		captured.body, captured.err = response.Body()
		if captured.err != nil {
			captured.err = fmt.Errorf("%w: read provider response body: %w", ErrProviderTransport, captured.err)
		}
		if captured.err == nil && len(captured.body) > maxScheduleResponseBytes {
			captured.err = fmt.Errorf("CGV provider response exceeds %d bytes", maxScheduleResponseBytes)
			captured.body = nil
		}
	}
	adapter.scheduleResponseMu.Lock()
	if len(adapter.providerResponses) >= maxCapturedProviderResponses {
		if len(adapter.providerResponses) == maxCapturedProviderResponses {
			adapter.providerResponses = append(adapter.providerResponses, capturedProviderResponse{
				path: path, err: errors.New("CGV provider response capture limit exceeded"),
			})
		}
		adapter.scheduleResponseMu.Unlock()
		return
	}
	adapter.providerResponses = append(adapter.providerResponses, captured)
	adapter.scheduleResponseMu.Unlock()
	if adapter.logger != nil {
		adapter.logger.InfoContext(adapter.ctx, "CGV provider response captured",
			"event", "cgv.schedule.provider_response.captured",
			"scenario", "schedule_collection",
			"operation", "capture_provider_response",
			"outcome", "completed",
			"request_url", captured.requestURL,
			"status", captured.status,
			"response_bytes", len(captured.body),
			"error", captured.err,
		)
	}
}

func (adapter *Adapter) resetProviderResponses() {
	adapter.scheduleResponseMu.Lock()
	adapter.providerResponses = nil
	adapter.scheduleResponseMu.Unlock()
}

func (adapter *Adapter) waitForScheduleResponseReady(targetDate string, timeout time.Duration) error {
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		adapter.scheduleResponseMu.Lock()
		ready := false
		for _, response := range adapter.providerResponses {
			if scheduleResponseTargetsDate(response, targetDate) {
				ready = true
				break
			}
		}
		adapter.scheduleResponseMu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-adapter.ctx.Done():
			return context.Cause(adapter.ctx)
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func scheduleResponseTargetsDate(response capturedProviderResponse, targetDate string) bool {
	if response.path != scheduleResponsePath {
		return false
	}
	normalizedTarget := strings.ReplaceAll(strings.TrimSpace(targetDate), "-", "")
	if parsed, err := url.Parse(response.requestURL); err == nil {
		for _, values := range parsed.Query() {
			for _, value := range values {
				if strings.ReplaceAll(strings.TrimSpace(value), "-", "") == normalizedTarget {
					return true
				}
			}
		}
	}
	rows, err := parseScheduleResponse(response.body)
	if err != nil {
		return false
	}
	for _, row := range rows {
		if strings.ReplaceAll(row.Date, "-", "") == normalizedTarget {
			return true
		}
	}
	return false
}

func (adapter *Adapter) captureScheduleRows() ([]providerScheduleRow, error) {
	captures := adapter.takeProviderResponses(scheduleResponsePath)
	if len(captures) == 0 {
		return nil, errScheduleResponseMissing
	}
	var rows []providerScheduleRow
	seen := make(map[string]struct{})
	for _, captured := range captures {
		if captured.err != nil {
			return nil, adapter.handleProviderFailure(captured.err)
		}
		parsed, err := parseScheduleResponse(captured.body)
		if err != nil {
			return nil, err
		}
		for _, row := range parsed {
			key := showtimeSourceKey(row.SiteNo, row.Date, row.AuditoriumNo, row.Sequence)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func providerHTTPError(status int) error {
	switch {
	case status >= 300 && status <= 399:
		return fmt.Errorf("%w: HTTP %d", ErrUIContractChanged, status)
	case status == 400 || status == 422:
		return fmt.Errorf("%w: HTTP %d", ErrProviderInvalidResult, status)
	case status == 401:
		return fmt.Errorf("%w: HTTP %d", ErrAuthenticationRequired, status)
	case status == 403:
		return fmt.Errorf("%w: HTTP %d", ErrProviderAccessBlocked, status)
	case status == 408 || status == 504:
		return fmt.Errorf("%w: HTTP %d", context.DeadlineExceeded, status)
	case status == 429:
		return fmt.Errorf("%w: HTTP %d", ErrProviderThrottled, status)
	case status >= 500 && status <= 599:
		return fmt.Errorf("%w: HTTP %d", ErrProviderServerError, status)
	case status >= 400 && status <= 499:
		return fmt.Errorf("%w: HTTP %d", ErrUIContractChanged, status)
	default:
		return fmt.Errorf("%w: HTTP %d", ErrProviderTransport, status)
	}
}

func (adapter *Adapter) takeProviderResponses(paths ...string) []capturedProviderResponse {
	adapter.scheduleResponseMu.Lock()
	defer adapter.scheduleResponseMu.Unlock()
	allowed := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		allowed[path] = struct{}{}
	}
	var captures []capturedProviderResponse
	for _, response := range adapter.providerResponses {
		if _, ok := allowed[response.path]; ok {
			captures = append(captures, response)
		}
	}
	adapter.providerResponses = nil
	return captures
}
