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

	catalogpb "github.com/cineko-org/contracts/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/gen/go/cineko/probe"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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
		if request.Header.Get("Accept") != "application/json" {
			t.Errorf("common headers = %v", request.Header)
		}
		switch request.URL.Path {
		case "/v1/probes/register":
			assertRequestHeaders(t, request, "bootstrap", "install_01", "")
			response := &probepb.RegisterResponse{}
			response.SetProbeId("probe_01")
			response.SetNetworkId("network_01")
			response.SetAccessToken("access_01")
			response.SetTokenExpiresAt(timestamppb.New(time.Date(2026, 8, 11, 5, 0, 0, 0, time.UTC)))
			response.SetHeartbeatIntervalSeconds(30)
			writeProtoJSON(t, writer, response)
		case "/v1/probes/probe_01/heartbeat":
			assertRequestHeaders(t, request, "access_01", "", "")
			response := &probepb.HeartbeatResponse{}
			response.SetServerTime(timestamppb.New(time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC)))
			response.SetMinimumRuntimeVersion("1.0.0")
			response.SetMinimumBrowserRevision("1228")
			writeProtoJSON(t, writer, response)
		case "/v1/probes/probe_01/assignments:claim":
			assertRequestHeaders(t, request, "access_01", "", "")
			assignment := &probepb.AssignmentLease{}
			assignment.SetAssignmentId("assignment_01")
			assignment.SetLeaseToken("lease_01")
			assignment.SetLeaseExpiresAt(timestamppb.New(time.Date(2026, 8, 10, 5, 2, 0, 0, time.UTC)))
			assignment.SetNotBefore(timestamppb.New(time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC)))
			assignment.SetDeadline(timestamppb.New(time.Date(2026, 8, 10, 5, 10, 0, 0, time.UTC)))
			assignment.SetTask(scheduleTaskForTest())
			response := &probepb.ClaimAssignmentResponse{}
			response.SetAssignment(assignment)
			writeProtoJSON(t, writer, response)
		case "/v1/assignments/assignment_01/heartbeat":
			assertRequestHeaders(t, request, "access_01", "", "lease_01")
			response := &probepb.HeartbeatAssignmentResponse{}
			response.SetLeaseExpiresAt(timestamppb.New(time.Date(2026, 8, 10, 5, 3, 0, 0, time.UTC)))
			writeProtoJSON(t, writer, response)
		case "/v1/assignments/assignment_01/result":
			assertRequestHeaders(t, request, "access_01", "run_01", "lease_01")
			receipt := &observationpb.ResultReceipt{}
			receipt.SetAssignmentId("assignment_01")
			receipt.SetRunId("run_01")
			receipt.SetContentHash("hash")
			receipt.SetAccepted(&observationpb.Accepted{})
			writeProtoJSON(t, writer, receipt)
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
	if !api.AssignmentClaimWaitsForAvailability() {
		t.Fatal("HTTP API claim did not advertise availability waiting")
	}
	registration := testRegistration()
	registered, err := api.Register(context.Background(), "bootstrap", registration)
	if err != nil || registered.GetProbeId() != "probe_01" {
		t.Fatalf("registration = %+v, %v", registered, err)
	}
	session := Session{ProbeID: registered.GetProbeId(), AccessToken: registered.GetAccessToken()}
	heartbeatRequest := &probepb.HeartbeatRequest{}
	heartbeatRequest.SetAvailableSlots(1)
	health := &probepb.ProbeHealth{}
	health.SetHealthy(&probepb.Healthy{})
	heartbeatRequest.SetHealth(health)
	heartbeat, err := api.HeartbeatProbe(context.Background(), session, heartbeatRequest)
	if err != nil || heartbeat.GetMinimumBrowserRevision() != "1228" {
		t.Fatalf("heartbeat = %+v, %v", heartbeat, err)
	}
	assignmentResponse, err := api.ClaimAssignment(context.Background(), session)
	assignment := assignmentResponse.GetAssignment()
	if err != nil || assignment == nil || assignment.GetAssignmentId() != "assignment_01" {
		t.Fatalf("claim = %+v, %v", assignmentResponse, err)
	}
	extension, err := api.HeartbeatAssignment(context.Background(), session, assignment)
	if err != nil || extension.GetLeaseExpiresAt().AsTime().Minute() != 3 {
		t.Fatalf("lease extension = %+v, %v", extension, err)
	}
	result := &observationpb.AssignmentResult{}
	result.SetRunId("run_01")
	result.SetStartedAt(timestamppb.Now())
	result.SetFinishedAt(timestamppb.Now())
	completed := &observationpb.Completed{}
	completed.SetCaptures([]*observationpb.Capture{})
	result.SetCompleted(completed)
	receipt, err := api.CommitResult(context.Background(), session, assignment, result)
	if err != nil || receipt.GetRunId() != "run_01" {
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
			writeAPIError(t, writer, http.StatusUnauthorized, "unauthorized", "no", false, "req_01")
		case "expired":
			writeAPIError(t, writer, http.StatusConflict, "lease_expired", "expired", false, "req_02")
		case "retryable":
			writeAPIError(t, writer, http.StatusServiceUnavailable, "unavailable", "later", true, "req_03")
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
		_, err := api.CommitResult(context.Background(), Session{}, &probepb.AssignmentLease{}, &observationpb.AssignmentResult{})
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
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(errorReader{})}, nil
	})
	if _, err := api.Register(context.Background(), "token", testRegistration()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("response read error = %v", err)
	}
	if _, err := api.newRequest(context.Background(), "bad\nmethod", "/", "", "", "", nil); err == nil {
		t.Fatal("invalid HTTP method accepted")
	}
	api.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	if err := api.request(context.Background(), http.MethodGet, "/", "", "", "", nil, nil); err != nil {
		t.Fatalf("empty successful response = %v", err)
	}
	if err := decodeProtoJSON([]byte(`{} {}`), &probepb.RegisterResponse{}); err == nil {
		t.Fatal("multiple response values accepted")
	}
	if err := decodeProtoJSON([]byte(`{"probe_id":"probe","unknown":true}`), &probepb.RegisterResponse{}); err == nil {
		t.Fatal("unknown inbound field accepted")
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

func writeProtoJSON(t *testing.T, writer http.ResponseWriter, message proto.Message) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	encoded, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(message)
	if err != nil {
		t.Errorf("encode response: %v", err)
		return
	}
	_, _ = writer.Write(encoded)
}

func writeAPIError(t *testing.T, writer http.ResponseWriter, status int, code, message string, retryable bool, requestID string) {
	writer.WriteHeader(status)
	errorValue := &commonpb.APIError{}
	errorValue.SetCode(code)
	errorValue.SetMessage(message)
	errorValue.SetRetryable(retryable)
	errorValue.SetRequestId(requestID)
	response := &commonpb.APIErrorResponse{}
	response.SetError(errorValue)
	writeProtoJSON(t, writer, response)
}

func scheduleTaskForTest() *observationpb.AssignmentTask {
	theater := &catalogpb.Theater{}
	theater.SetId("theater_01")
	theater.SetProviderId("cgv")
	theater.SetSourceKey("서울/용산아이파크몰")
	theater.SetRegion("서울")
	theater.SetName("용산아이파크몰")
	date := &commonpb.LocalDate{}
	date.SetYear(2026)
	date.SetMonth(8)
	date.SetDay(20)
	schedule := &observationpb.ScheduleTask{}
	schedule.SetTheater(theater)
	schedule.SetTargetDates([]*commonpb.LocalDate{date})
	schedule.SetLocale("ko-KR")
	schedule.SetTimeZone("Asia/Seoul")
	egress := &commonpb.EgressPolicy{}
	egress.SetManagedScan(&commonpb.ManagedScanEgress{})
	task := &observationpb.AssignmentTask{}
	task.SetEgress(egress)
	task.SetSchedule(schedule)
	return task
}

func testRegistration() *probepb.RegisterRequest {
	registration := &probepb.RegisterRequest{}
	registration.SetInstallationId("install_01")
	kind := &probepb.ProbeKind{}
	kind.SetContainer(&probepb.ContainerProbe{})
	registration.SetKind(kind)
	capability := &observationpb.Capability{}
	capability.SetScheduleCapture(&observationpb.ScheduleCapture{})
	registration.SetCapabilities([]*observationpb.Capability{capability})
	registration.SetMaxConcurrency(1)
	runtime := &commonpb.Runtime{}
	runtime.SetComponentVersion("1.0.0")
	runtime.SetBrowserRevision("1228")
	runtime.SetPlatform("linux")
	runtime.SetArchitecture("amd64")
	registration.SetRuntime(runtime)
	return registration
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
