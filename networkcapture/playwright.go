package networkcapture

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// ShouldCapturePlaywrightRequest avoids materializing successful browser
// bodies outside debug mode while retaining failures and 4xx/5xx responses.
func ShouldCapturePlaywrightRequest(store *Store, request playwright.Request, failed bool) bool {
	if store == nil || request == nil {
		return false
	}
	if store.DebugEnabled() {
		return true
	}
	if failure := request.Failure(); failure != nil {
		return playwrightExpectedOutcome(failure) == ""
	}
	if failed {
		return true
	}
	response, err := request.ExistingResponse()
	if err != nil {
		return true
	}
	return response != nil && response.Status() >= 400
}

// PlaywrightRecord snapshots a completed browser request. Response.Body is the
// representation exposed to the page after content decoding; the encoded wire
// size remains available in Response.Bytes.
//
//nolint:gocyclo,cyclop // A single immutable snapshot must preserve every optional Playwright request and response field.
func PlaywrightRecord(request playwright.Request, failed bool) Record {
	now := time.Now()
	if request == nil {
		return Record{Exchange: Exchange{
			Transport: "chromium", StartedAt: now, CompletedAt: now,
			Outcome: "failed", Error: "browser request is nil", Request: Request{},
		}}
	}
	timing := request.Timing()
	startedAt := now
	completedAt := now
	var capturedTiming *Timing
	if timing != nil {
		if timing.StartTime > 0 {
			startedAt = time.UnixMilli(int64(timing.StartTime))
		}
		if timing.ResponseEnd >= 0 {
			completedAt = startedAt.Add(time.Duration(timing.ResponseEnd * float64(time.Millisecond)))
		}
		capturedTiming = &Timing{
			StartMillis: timing.StartTime, DomainLookupStart: timing.DomainLookupStart,
			DomainLookupEnd: timing.DomainLookupEnd, ConnectStart: timing.ConnectStart,
			SecureConnectionStart: timing.SecureConnectionStart, ConnectEnd: timing.ConnectEnd,
			RequestStart: timing.RequestStart, ResponseStart: timing.ResponseStart,
			ResponseEnd: timing.ResponseEnd,
		}
	}
	requestHeaders, _ := request.HeadersArray()
	requestBody, requestBodyErr := request.PostDataBuffer()
	requestCapture := Request{
		Method: request.Method(), URL: request.URL(), Headers: PlaywrightHeaders(requestHeaders),
		ResourceType: request.ResourceType(), Navigation: request.IsNavigationRequest(), Timing: capturedTiming,
	}
	if redirected := request.RedirectedFrom(); redirected != nil {
		requestCapture.RedirectedFrom = redirected.URL()
	}
	if redirected := request.RedirectedTo(); redirected != nil {
		requestCapture.RedirectedTo = redirected.URL()
	}
	if sizes, err := request.Sizes(); err == nil && sizes != nil {
		requestCapture.Bytes = int64(sizes.RequestHeadersSize + sizes.RequestBodySize)
	}
	record := Record{
		Exchange: Exchange{
			Transport: "chromium", StartedAt: startedAt, CompletedAt: completedAt,
			Request: requestCapture,
		},
		RequestBody: requestBody, RequestContentType: playwrightHeaderValue(requestHeaders, "content-type"),
		RequestRepresentation: "application",
	}
	response, responseLookupErr := request.ExistingResponse()
	if responseLookupErr == nil && response == nil && !failed {
		response, responseLookupErr = request.Response()
	}
	var responseCaptureErr error
	if response != nil {
		responseHeaders, headersErr := response.HeadersArray()
		responseBody, bodyErr := response.Body()
		protocol, protocolErr := response.HttpVersion()
		server, serverErr := response.ServerAddr()
		capturedResponse := &Response{
			Status: response.Status(), StatusText: response.StatusText(), Protocol: protocol,
			Headers: PlaywrightHeaders(responseHeaders), FromServiceWorker: response.FromServiceWorker(),
		}
		if sizes, err := request.Sizes(); err == nil && sizes != nil {
			capturedResponse.Bytes = int64(sizes.ResponseHeadersSize + sizes.ResponseBodySize)
		}
		if server != nil {
			capturedResponse.ServerAddress = server.IpAddress
			capturedResponse.ServerPort = server.Port
		}
		record.Response = capturedResponse
		record.ResponseBody = responseBody
		record.ResponseContentType = playwrightHeaderValue(responseHeaders, "content-type")
		record.ResponseRepresentation = "decoded"
		responseCaptureErr = errors.Join(headersErr, bodyErr, protocolErr, serverErr)
	}
	if captureErr := errors.Join(requestBodyErr, responseLookupErr, responseCaptureErr); captureErr != nil {
		record.CaptureError = captureErr.Error()
	}
	status := 0
	if record.Response != nil {
		status = record.Response.Status
	}
	rawErr := playwrightRequestError(request, failed, status)
	switch outcome := playwrightExpectedOutcome(rawErr); {
	case outcome != "":
		record.Outcome = outcome
	case rawErr != nil:
		record.Outcome = "failed"
		record.Error = rawErr.Error()
	default:
		record.Outcome = "succeeded"
	}
	return record
}

func PlaywrightHeaders(headers []playwright.NameValue) []Header {
	result := make([]Header, 0, len(headers))
	for _, header := range headers {
		result = append(result, Header{Name: header.Name, Value: header.Value})
	}
	return result
}

func playwrightHeaderValue(headers []playwright.NameValue, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

func playwrightRequestError(request playwright.Request, failed bool, status int) error {
	if requestErr := request.Failure(); requestErr != nil {
		return requestErr
	}
	if failed {
		return errors.New("browser request failed")
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

func playwrightExpectedOutcome(err error) string {
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
