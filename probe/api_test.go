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

	"buf.build/go/protovalidate"
	catalogpb "github.com/cineko-org/contracts/v3/gen/go/cineko/catalog"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/v3/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/v3/gen/go/cineko/probe"
	servicepb "github.com/cineko-org/contracts/v3/gen/go/cineko/service"
	"github.com/cineko-org/probe/v2/internal/provider/cgv"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
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
			assertProtoRequest(t, request, &probepb.ClaimAssignmentRequest{})
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
			input := &probepb.HeartbeatAssignmentRequest{}
			assertProtoRequest(t, request, input)
			if input.GetAssignmentId() != "assignment_01" || input.GetLeaseToken() != "lease_01" {
				t.Errorf("assignment heartbeat request = %+v", input)
			}
			response := &probepb.HeartbeatAssignmentResponse{}
			response.SetLeaseExpiresAt(timestamppb.New(time.Date(2026, 8, 10, 5, 3, 0, 0, time.UTC)))
			writeProtoJSON(t, writer, response)
		case "/v1/assignments/assignment_01/result":
			assertRequestHeaders(t, request, "access_01", "run_01", "lease_01")
			input := &probepb.SubmitAssignmentResultRequest{}
			assertProtoRequest(t, request, input)
			if input.GetAssignmentId() != "assignment_01" || input.GetLeaseToken() != "lease_01" ||
				input.GetResult().GetRunId() != "run_01" {
				t.Errorf("assignment result request = %+v", input)
			}
			receipt := &observationpb.ResultReceipt{}
			receipt.SetAssignmentId("assignment_01")
			receipt.SetRunId("run_01")
			receipt.SetContentHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			receipt.SetAccepted(&observationpb.Accepted{})
			response := &servicepb.SubmitAssignmentResultResponse{}
			response.SetReceipt(receipt)
			writeProtoJSON(t, writer, response)
		case "/v1/probes/probe_01/disconnect":
			assertRequestHeaders(t, request, "access_01", "", "")
			assertProtoRequest(t, request, &servicepb.DisconnectRequest{})
			writeProtoJSON(t, writer, &servicepb.DisconnectResponse{})
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
	schedule := &observationpb.ScheduleCaptures{}
	schedule.SetCaptures([]*observationpb.Capture{{}})
	completed.SetSchedule(schedule)
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
			response := &probepb.ClaimAssignmentResponse{}
			noAssignment := &probepb.NoAssignment{}
			noAssignment.SetRetryAt(timestamppb.New(time.Now().Add(time.Minute)))
			response.SetNoAssignment(noAssignment)
			writeProtoJSON(t, writer, response)
		case "legacy-204":
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
			_, _ = writer.Write([]byte(`{"unexpected":true}`))
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
	if err != nil || assignment.GetNoAssignment() == nil {
		t.Fatalf("empty claim = %+v, %v", assignment, err)
	}
	api.baseURL.RawQuery = "case=legacy-204"
	if _, err := api.ClaimAssignment(context.Background(), Session{ProbeID: "probe", AccessToken: "token"}); err == nil {
		t.Fatal("legacy 204 no-assignment response accepted")
	}
	for name, want := range map[string]error{"unauthorized": ErrUnauthorized, "expired": ErrLeaseExpired} {
		api.baseURL.RawQuery = "case=" + name
		lease := &probepb.AssignmentLease{}
		lease.SetAssignmentId("assignment_01")
		lease.SetLeaseToken("lease_01")
		_, err := api.CommitResult(context.Background(), Session{}, lease, assignmentResultForTest())
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
	if err := api.request(context.Background(), http.MethodGet, "/", "", "", "", nil, nil); err == nil {
		t.Fatal("unexpected response body accepted without an output envelope")
	}
}

func TestHTTPAPIClaimsGlobalCatalogAssignment(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		lease := &probepb.AssignmentLease{}
		lease.SetAssignmentId("assignment_catalog")
		lease.SetLeaseToken("lease_catalog")
		lease.SetLeaseExpiresAt(timestamppb.New(time.Date(2026, 8, 23, 5, 2, 0, 0, time.UTC)))
		lease.SetNotBefore(timestamppb.New(time.Date(2026, 8, 23, 5, 0, 0, 0, time.UTC)))
		lease.SetDeadline(timestamppb.New(time.Date(2026, 8, 23, 5, 10, 0, 0, time.UTC)))
		lease.SetTask(catalogTaskForTest())
		response := &probepb.ClaimAssignmentResponse{}
		response.SetAssignment(lease)
		writeProtoJSON(t, writer, response)
	}))
	defer server.Close()

	api, err := NewHTTPAPI(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := api.ClaimAssignment(context.Background(), Session{ProbeID: "probe", AccessToken: "token"})
	if err != nil {
		t.Fatalf("claim global catalog assignment: %v", err)
	}
	if response.GetAssignment().GetTask().GetCatalog().GetProviderId() != cgv.ProviderCGV {
		t.Fatalf("catalog assignment = %+v", response)
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
	if err := api.request(context.Background(), "bad\nmethod", "/", "", "", "", nil, nil); err == nil {
		t.Fatal("request accepted an invalid HTTP method")
	}
	if _, err := api.newRequest(
		context.Background(), http.MethodPut, "/", "", "", "", &probepb.HeartbeatAssignmentRequest{},
	); err == nil {
		t.Fatal("invalid generated request accepted")
	}
	if _, err := api.newRequest(
		context.Background(), http.MethodPut, "/", "", "", "", wrapperspb.String("\xff"),
	); err == nil {
		t.Fatal("invalid UTF-8 protobuf string accepted")
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

func assignmentResultForTest() *observationpb.AssignmentResult {
	result := &observationpb.AssignmentResult{}
	result.SetRunId("run_01")
	result.SetStartedAt(timestamppb.Now())
	result.SetFinishedAt(timestamppb.Now())
	completed := &observationpb.Completed{}
	schedule := &observationpb.ScheduleCaptures{}
	schedule.SetCaptures([]*observationpb.Capture{{}})
	completed.SetSchedule(schedule)
	result.SetCompleted(completed)
	return result
}

func assertRequestHeaders(t *testing.T, request *http.Request, bearer, idempotency, lease string) {
	t.Helper()
	if request.Header.Get("Authorization") != "Bearer "+bearer ||
		request.Header.Get("Idempotency-Key") != idempotency ||
		request.Header.Get("X-Cineko-Lease-Token") != lease {
		t.Errorf("request headers = %v", request.Header)
	}
}

func assertProtoRequest(t *testing.T, request *http.Request, message proto.Message) {
	t.Helper()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readRequestBody(t, request), message); err != nil {
		t.Fatalf("decode %T request: %v", message, err)
	}
	if err := protovalidate.Validate(message); err != nil {
		t.Fatalf("validate %T request: %v", message, err)
	}
}

func readRequestBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return payload
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
	theater.SetId(cgv.CatalogID(cgv.ProviderCGV, "theater", "0056"))
	theater.SetProviderId("cgv")
	theater.SetIdentity(cgv.NewTheaterIdentity("0056"))
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

func catalogTaskForTest() *observationpb.AssignmentTask {
	catalog := &observationpb.CatalogTask{}
	catalog.SetProviderId(cgv.ProviderCGV)
	catalog.SetLocale("ko-KR")
	catalog.SetTimeZone("Asia/Seoul")
	egress := &commonpb.EgressPolicy{}
	egress.SetManagedScan(&commonpb.ManagedScanEgress{})
	task := &observationpb.AssignmentTask{}
	task.SetEgress(egress)
	task.SetCatalog(catalog)
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
