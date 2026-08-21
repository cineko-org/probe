package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	commonpb "github.com/cineko-org/contracts/gen/go/cineko/common"
	observationpb "github.com/cineko-org/contracts/gen/go/cineko/observation"
	probepb "github.com/cineko-org/contracts/gen/go/cineko/probe"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const maxResponseBody = 4 << 20

var (
	ErrUnauthorized = errors.New("probe API authentication failed")
	ErrLeaseExpired = errors.New("probe assignment lease expired")
)

type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Retryable  bool
	RequestID  string
}

func (apiError *APIError) Error() string {
	return fmt.Sprintf("Central API %s (%d, request %s)", apiError.Code, apiError.StatusCode, apiError.RequestID)
}

type API interface {
	Register(context.Context, string, *probepb.RegisterRequest) (*probepb.RegisterResponse, error)
	HeartbeatProbe(context.Context, Session, *probepb.HeartbeatRequest) (*probepb.HeartbeatResponse, error)
	DisconnectProbe(context.Context, Session) error
	ClaimAssignment(context.Context, Session) (*probepb.ClaimAssignmentResponse, error)
	HeartbeatAssignment(context.Context, Session, *probepb.AssignmentLease) (*probepb.HeartbeatAssignmentResponse, error)
	CommitResult(
		context.Context,
		Session,
		*probepb.AssignmentLease,
		*observationpb.AssignmentResult,
	) (*observationpb.ResultReceipt, error)
}

type Session struct {
	ProbeID     string
	AccessToken string
}

type HTTPAPI struct {
	baseURL *url.URL
	client  *http.Client
}

// AssignmentClaimWaitsForAvailability reports that the Central endpoint holds
// an empty claim until an assignment notification or the bounded wait expires.
// Runtime uses this to avoid adding a second random delay after the long poll.
func (*HTTPAPI) AssignmentClaimWaitsForAvailability() bool { return true }

func NewHTTPAPI(rawBaseURL string, client *http.Client) (*HTTPAPI, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.User != nil ||
		(baseURL.Scheme != "https" && (baseURL.Scheme != "http" || !isLoopbackHost(baseURL.Hostname()))) {
		return nil, errors.New("central URL must be HTTPS or loopback HTTP without embedded credentials")
	}
	baseURL.RawQuery = ""
	baseURL.Fragment = ""
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &HTTPAPI{baseURL: baseURL, client: client}, nil
}

func (api *HTTPAPI) Register(
	ctx context.Context,
	credential string,
	input *probepb.RegisterRequest,
) (*probepb.RegisterResponse, error) {
	output := &probepb.RegisterResponse{}
	installationID := ""
	if input != nil {
		installationID = input.GetInstallationId()
	}
	err := api.request(
		ctx, http.MethodPost, "/v1/probes/register", credential, installationID, "", input, output,
	)
	return output, err
}

func (api *HTTPAPI) HeartbeatProbe(
	ctx context.Context,
	session Session,
	input *probepb.HeartbeatRequest,
) (*probepb.HeartbeatResponse, error) {
	output := &probepb.HeartbeatResponse{}
	err := api.request(
		ctx, http.MethodPut, "/v1/probes/"+url.PathEscape(session.ProbeID)+"/heartbeat",
		session.AccessToken, "", "", input, output,
	)
	return output, err
}

func (api *HTTPAPI) DisconnectProbe(ctx context.Context, session Session) error {
	return api.request(
		ctx, http.MethodPost, "/v1/probes/"+url.PathEscape(session.ProbeID)+"/disconnect",
		session.AccessToken, "", "", nil, nil,
	)
}

func (api *HTTPAPI) ClaimAssignment(
	ctx context.Context,
	session Session,
) (*probepb.ClaimAssignmentResponse, error) {
	output := &probepb.ClaimAssignmentResponse{}
	err := api.request(
		ctx, http.MethodPost, "/v1/probes/"+url.PathEscape(session.ProbeID)+"/assignments:claim",
		session.AccessToken, "", "", nil, output,
	)
	if errors.Is(err, errNoContent) {
		return nil, nil
	}
	return output, err
}

func (api *HTTPAPI) HeartbeatAssignment(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
) (*probepb.HeartbeatAssignmentResponse, error) {
	output := &probepb.HeartbeatAssignmentResponse{}
	err := api.request(
		ctx, http.MethodPut, "/v1/assignments/"+url.PathEscape(assignment.GetAssignmentId())+"/heartbeat",
		session.AccessToken, "", assignment.GetLeaseToken(), nil, output,
	)
	return output, err
}

func (api *HTTPAPI) CommitResult(
	ctx context.Context,
	session Session,
	assignment *probepb.AssignmentLease,
	result *observationpb.AssignmentResult,
) (*observationpb.ResultReceipt, error) {
	output := &observationpb.ResultReceipt{}
	err := api.request(
		ctx, http.MethodPut, "/v1/assignments/"+url.PathEscape(assignment.GetAssignmentId())+"/result",
		session.AccessToken, result.GetRunId(), assignment.GetLeaseToken(), result, output,
	)
	return output, err
}

var errNoContent = errors.New("central returned no content")

func (api *HTTPAPI) request(
	ctx context.Context,
	method string,
	path string,
	bearer string,
	idempotencyKey string,
	leaseToken string,
	input proto.Message,
	output proto.Message,
) error {
	request, err := api.newRequest(ctx, method, path, bearer, idempotencyKey, leaseToken, input)
	if err != nil {
		return err
	}
	response, err := api.client.Do(request)
	if err != nil {
		return fmt.Errorf("call Central API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("read Central API response: %w", err)
	}
	if len(contents) > maxResponseBody {
		return errors.New("central API response exceeds size limit")
	}
	if response.StatusCode == http.StatusNoContent {
		if output != nil {
			return errNoContent
		}
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAPIError(response, contents)
	}
	if output == nil {
		if len(bytes.TrimSpace(contents)) != 0 {
			return errors.New("central API returned an unexpected response body")
		}
		return nil
	}
	if err := decodeProtoJSON(contents, output); err != nil {
		return fmt.Errorf("decode Central API response: %w", err)
	}
	return nil
}

func (api *HTTPAPI) newRequest(
	ctx context.Context,
	method string,
	path string,
	bearer string,
	idempotencyKey string,
	leaseToken string,
	input proto.Message,
) (*http.Request, error) {
	var body io.Reader
	if input != nil {
		encoded, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode Central API request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := *api.baseURL
	endpoint.Path = strings.TrimRight(api.baseURL.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create Central API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if leaseToken != "" {
		request.Header.Set("X-Cineko-Lease-Token", leaseToken)
	}
	return request, nil
}

func decodeAPIError(response *http.Response, contents []byte) error {
	envelope := &commonpb.APIErrorResponse{}
	if err := decodeProtoJSON(contents, envelope); err != nil || envelope.GetError() == nil ||
		strings.TrimSpace(envelope.GetError().GetCode()) == "" {
		return fmt.Errorf("central API returned HTTP %d with an invalid error envelope", response.StatusCode)
	}
	apiError := &APIError{
		StatusCode: response.StatusCode, Code: envelope.GetError().GetCode(), Message: envelope.GetError().GetMessage(),
		Retryable: envelope.GetError().GetRetryable(), RequestID: envelope.GetError().GetRequestId(),
	}
	switch apiError.Code {
	case "unauthorized":
		return errors.Join(ErrUnauthorized, apiError)
	case "lease_expired":
		return errors.Join(ErrLeaseExpired, apiError)
	default:
		return apiError
	}
}

func decodeProtoJSON(contents []byte, output proto.Message) error {
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(contents, output); err != nil {
		return err
	}
	if err := protovalidate.Validate(output); err != nil {
		return err
	}
	return nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
