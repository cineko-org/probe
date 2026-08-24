package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPServerMiddlewarePropagatesRequestIDAndLogsCompletion(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/readyz?secret=must-not-be-a-route", strings.NewReader("ping"))
	request.Header.Set(RequestIDHeader, "incoming-123")
	response := httptest.NewRecorder()
	HTTPServerMiddleware(logger, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := RequestID(request.Context()); got != "incoming-123" {
			t.Fatalf("context request ID = %q", got)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "ping" {
			t.Fatalf("request body = %q", body)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("pong"))
	})).ServeHTTP(response, request)

	if got := response.Header().Get(RequestIDHeader); got != "incoming-123" {
		t.Fatalf("response request ID = %q", got)
	}
	if response.Code != http.StatusCreated || response.Body.String() != "pong" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	record := decodeHTTPLog(t, output.String())
	assertLogFields(t, record, map[string]any{
		"event":          "http.server.request.completed",
		"service":        "probe",
		"request_id":     "incoming-123",
		"method":         http.MethodPost,
		"route":          "/readyz",
		"path":           "/readyz",
		"status":         float64(http.StatusCreated),
		"outcome":        "succeeded",
		"request_bytes":  float64(4),
		"response_bytes": float64(4),
	})
	if _, ok := record["error"]; ok {
		t.Fatalf("success log unexpectedly contains error: %#v", record)
	}
}

func TestHTTPServerMiddlewareGeneratesRequestIDAndMarksHTTPFailures(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/missing", nil)
	HTTPServerMiddleware(logger, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	})).ServeHTTP(response, request)

	requestID := response.Header().Get(RequestIDHeader)
	if !strings.HasPrefix(requestID, "req_") {
		t.Fatalf("generated request ID = %q", requestID)
	}
	record := decodeHTTPLog(t, output.String())
	if record["request_id"] != requestID {
		t.Fatalf("logged request ID = %#v, response = %q", record["request_id"], requestID)
	}
	if record["level"] != "ERROR" || record["outcome"] != "failed" {
		t.Fatalf("failure record = %#v", record)
	}
	errorMessage, ok := record["error"].(string)
	if !ok || !strings.Contains(errorMessage, "HTTP 404") {
		t.Fatalf("failure error = %#v", record["error"])
	}
}

func TestHTTPClientTransportPropagatesContextAndLogsResponseBytes(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	transport := HTTPClientTransport(logger, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get(RequestIDHeader); got != "ctx-456" {
			t.Errorf("request ID header = %q", got)
		}
		if got := RequestID(request.Context()); got != "ctx-456" {
			t.Errorf("request context ID = %q", got)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("response")),
			ContentLength: int64(len("response")),
		}, nil
	}))
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(WithRequestID(context.Background(), "ctx-456"), http.MethodGet, "https://provider.example/v1/catalog", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	record := decodeHTTPLog(t, output.String())
	assertLogFields(t, record, map[string]any{
		"event":          "http.client.request.completed",
		"service":        "probe",
		"request_id":     "ctx-456",
		"method":         http.MethodGet,
		"route":          "/v1/catalog",
		"path":           "/v1/catalog",
		"status":         float64(http.StatusOK),
		"response_bytes": float64(len("response")),
		"outcome":        "succeeded",
	})
}

func TestHTTPClientTransportLogsRawTransportErrors(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	wantErr := errors.New("dial failed")
	client := &http.Client{Transport: HTTPClientTransport(logger, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	}))}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://provider.example/v1/catalog/1", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("client error = %v", err)
	}
	record := decodeHTTPLog(t, output.String())
	if record["level"] != "ERROR" || record["outcome"] != "failed" {
		t.Fatalf("transport failure record = %#v", record)
	}
	if record["error"] != wantErr.Error() {
		t.Fatalf("raw transport error = %#v", record["error"])
	}
	if record["request_bytes"] != float64(len("body")) {
		t.Fatalf("request bytes = %#v", record["request_bytes"])
	}
	if record["status"] != float64(0) || record["response_bytes"] != float64(0) {
		t.Fatalf("transport status/response bytes = %#v/%#v", record["status"], record["response_bytes"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func decodeHTTPLog(t *testing.T, raw string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one HTTP log line, got %d: %q", len(lines), raw)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode log %q: %v", lines[0], err)
	}
	return record
}

func assertLogFields(t *testing.T, record map[string]any, expected map[string]any) {
	t.Helper()
	for key, want := range expected {
		if got := record[key]; got != want {
			t.Errorf("log[%q] = %#v, want %#v; record = %#v", key, got, want, record)
		}
	}
	if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
		t.Errorf("duration_ms = %#v", record["duration_ms"])
	}
}
