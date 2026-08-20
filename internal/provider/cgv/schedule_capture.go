package cgv

import (
	"errors"
	"fmt"

	"github.com/mxschmitt/playwright-go"
)

type capturedProviderResponse struct {
	path   string
	status int
	body   []byte
	err    error
}

func (adapter *Adapter) captureProviderResponse(response playwright.Response) {
	if response == nil {
		return
	}
	path, ok := providerResponsePath(response.URL())
	if !ok {
		return
	}
	captured := capturedProviderResponse{path: path, status: response.Status()}
	if captured.status < 200 || captured.status > 299 {
		captured.err = providerHTTPError(captured.status)
	} else {
		captured.body, captured.err = response.Body()
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
}

func (adapter *Adapter) resetProviderResponses() {
	adapter.scheduleResponseMu.Lock()
	adapter.providerResponses = nil
	adapter.scheduleResponseMu.Unlock()
}

func (adapter *Adapter) captureScheduleRows() ([]providerScheduleRow, error) {
	captures := adapter.takeProviderResponses(scheduleResponsePath, legacyScheduleResponsePath)
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
	switch status {
	case 403:
		return fmt.Errorf("%w: HTTP %d", ErrProviderAccessBlocked, status)
	case 429:
		return fmt.Errorf("%w: HTTP %d", ErrProviderThrottled, status)
	default:
		return fmt.Errorf("CGV provider response returned HTTP %d", status)
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
