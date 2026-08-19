package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	central "github.com/cineko-org/contracts/v3"
)

func TestAPIErrorFormatsStableMetadata(t *testing.T) {
	err := (&APIError{StatusCode: 503, Code: "unavailable", RequestID: "request_01"}).Error()
	if err != "Central API unavailable (503, request request_01)" {
		t.Fatalf("Error() = %q", err)
	}
}

func TestHTTPAPIProbeLifecycleContract(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Cineko-Protocol") != central.ProtocolHeaderValue() ||
			request.Header.Get("Accept") != "application/json" {
			t.Errorf("common headers = %v", request.Header)
		}
		switch request.URL.Path {
		case "/v1/probes/register":
			assertRequestHeaders(t, request, "bootstrap", "install_01", "")
			writeJSON(writer, `{"probeId":"probe_01","networkId":"network_01","accessToken":"access_01","tokenExpiresAt":"2026-08-11T05:00:00Z","heartbeatIntervalSeconds":30}`)
		case "/v1/probes/probe_01/heartbeat":
			assertRequestHeaders(t, request, "access_01", "", "")
			writeJSON(writer, `{"serverTime":"2026-08-10T05:00:00Z","drain":false,"minimumRuntimeVersion":"1.0.0","minimumBrowserRevision":"1228"}`)
		case "/v1/probes/probe_01/assignments:claim":
			assertRequestHeaders(t, request, "access_01", "", "")
			writeJSON(writer, `{"assignmentId":"assignment_01","leaseToken":"lease_01","leaseExpiresAt":"2026-08-10T05:02:00Z","notBefore":"2026-08-10T05:00:00Z","deadline":"2026-08-10T05:10:00Z","task":{"kind":"cgv.schedule.capture.v2","theater":{"id":"theater_01","providerId":"cgv","sourceKey":"서울/용산아이파크몰","region":"서울","name":"용산아이파크몰"},"targetDates":["2026-08-20"],"locale":"ko-KR","timeZone":"Asia/Seoul","egressPolicyId":"scan_default"}}`)
		case "/v1/assignments/assignment_01/heartbeat":
			assertRequestHeaders(t, request, "access_01", "", "lease_01")
			writeJSON(writer, `{"leaseExpiresAt":"2026-08-10T05:03:00Z"}`)
		case "/v1/assignments/assignment_01/result":
			assertRequestHeaders(t, request, "access_01", "run_01", "lease_01")
			writeJSON(writer, `{"assignmentId":"assignment_01","runId":"run_01","contentHash":"hash","status":"completed"}`)
		case "/v1/probes/probe_01/disconnect":
			assertRequestHeaders(t, request, "access_01", "", "")
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	api, err := NewHTTPAPI(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	registration := testRegistration()
	registered, err := api.Register(context.Background(), "bootstrap", registration)
	if err != nil || registered.ProbeID != "probe_01" {
		t.Fatalf("registration = %+v, %v", registered, err)
	}
	session := Session{ProbeID: registered.ProbeID, AccessToken: registered.AccessToken}
	heartbeat, err := api.HeartbeatProbe(context.Background(), session, central.ProbeHeartbeatRequest{
		AvailableSlots: 1, Health: "healthy",
	})
	if err != nil || heartbeat.MinimumBrowserRevision != "1228" {
		t.Fatalf("heartbeat = %+v, %v", heartbeat, err)
	}
	assignment, err := api.ClaimAssignment(context.Background(), session)
	if err != nil || assignment == nil || assignment.AssignmentID != "assignment_01" {
		t.Fatalf("claim = %+v, %v", assignment, err)
	}
	extension, err := api.HeartbeatAssignment(context.Background(), session, *assignment)
	if err != nil || extension.LeaseExpiresAt.Minute() != 3 {
		t.Fatalf("lease extension = %+v, %v", extension, err)
	}
	receipt, err := api.CommitResult(context.Background(), session, *assignment, central.AssignmentResult{
		RunID: "run_01", Status: "completed", StartedAt: time.Now(), FinishedAt: time.Now(),
	})
	if err != nil || receipt.RunID != "run_01" {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
	if err := api.DisconnectProbe(context.Background(), session); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPAPIEmptyClaimAndErrors(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("case") {
		case "empty":
			writer.WriteHeader(http.StatusNoContent)
		case "unauthorized":
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":{"code":"unauthorized","message":"no","retryable":false,"requestId":"req_01"}}`))
		case "expired":
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"error":{"code":"lease_expired","message":"expired","retryable":false,"requestId":"req_02"}}`))
		case "retryable":
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"error":{"code":"unavailable","message":"later","retryable":true,"requestId":"req_03"}}`))
		case "bad-error":
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`not-json`))
		case "bad-success":
			_, _ = writer.Write([]byte(`not-json`))
		case "unexpected":
			_, _ = writer.Write([]byte(`{}`))
		case "large":
			_, _ = writer.Write(bytes.Repeat([]byte("x"), maxResponseBody+1))
		}
	}))
	defer server.Close()
	api, err := NewHTTPAPI(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	api.baseURL.RawQuery = "case=empty"
	assignment, err := api.ClaimAssignment(context.Background(), Session{ProbeID: "probe", AccessToken: "token"})
	if err != nil || assignment != nil {
		t.Fatalf("empty claim = %+v, %v", assignment, err)
	}
	for name, want := range map[string]error{"unauthorized": ErrUnauthorized, "expired": ErrLeaseExpired} {
		api.baseURL.RawQuery = "case=" + name
		_, err := api.CommitResult(context.Background(), Session{}, central.ClaimAssignmentResponse{}, central.AssignmentResult{})
		if !errors.Is(err, want) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	api.baseURL.RawQuery = "case=retryable"
	_, err = api.Register(context.Background(), "token", testRegistration())
	var apiError *APIError
	if !errors.As(err, &apiError) || !apiError.Retryable || apiError.RequestID != "req_03" || apiError.Message != "later" {
		t.Fatalf("retryable API error = %#v, %v", apiError, err)
	}
	for _, value := range []string{"bad-error", "bad-success", "large"} {
		api.baseURL.RawQuery = "case=" + value
		if _, err := api.Register(context.Background(), "token", testRegistration()); err == nil {
			t.Fatalf("case %s unexpectedly succeeded", value)
		}
	}
	api.baseURL.RawQuery = "case=unexpected"
	if err := api.DisconnectProbe(context.Background(), Session{}); err == nil {
		t.Fatal("unexpected disconnect body accepted")
	}
}

func TestHTTPAPIValidationAndTransportFailures(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "://", "ftp://example.com", "http://example.com", "https://user:pass@example.com"} {
		if _, err := NewHTTPAPI(value, nil); err == nil {
			t.Fatalf("invalid Central URL %q accepted", value)
		}
	}
	for _, value := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080", "https://central.example"} {
		if _, err := NewHTTPAPI(value, nil); err != nil {
			t.Fatalf("valid Central URL %q rejected: %v", value, err)
		}
	}
	api, err := NewHTTPAPI("https://central.example", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Register(context.Background(), "token", testRegistration()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("transport error = %v", err)
	}
	api.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(errorReader{}),
		}, nil
	})
	if _, err := api.Register(context.Background(), "token", testRegistration()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("response read error = %v", err)
	}
	if _, err := api.newRequest(
		context.Background(), "bad\nmethod", "/", "", "", "", nil,
	); err == nil {
		t.Fatal("invalid HTTP method accepted")
	}
	if _, err := api.newRequest(
		context.Background(), http.MethodPost, "/", "", "", "", make(chan int),
	); err == nil {
		t.Fatal("unencodable API request accepted")
	}
	if err := api.request(
		context.Background(), "bad\nmethod", "/", "", "", "", nil, nil,
	); err == nil {
		t.Fatal("request creation error was ignored")
	}
	api.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	if err := api.request(context.Background(), http.MethodGet, "/", "", "", "", nil, nil); err != nil {
		t.Fatalf("empty successful response = %v", err)
	}
	if err := decodeJSON([]byte(`{} {}`), &map[string]any{}); err == nil {
		t.Fatal("multiple response values accepted")
	}
}

func assertRequestHeaders(t *testing.T, request *http.Request, bearer, idempotency, lease string) {
	t.Helper()
	if request.Header.Get("Authorization") != "Bearer "+bearer ||
		request.Header.Get("Idempotency-Key") != idempotency ||
		request.Header.Get("X-Cineko-Lease-Token") != lease {
		t.Errorf("request headers = %v", request.Header)
	}
}

func writeJSON(writer http.ResponseWriter, value string) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(value))
}

func testRegistration() central.RegisterProbeRequest {
	return central.RegisterProbeRequest{
		InstallationID: "install_01", Kind: "container", Capabilities: []string{central.CapabilityCGVScheduleCapture},
		MaxConcurrency: 1,
		Runtime: central.Runtime{
			Version: "1.0.0", Protocol: central.ProtocolVersion, BrowserRevision: "1228", Platform: "linux", Arch: "amd64",
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
