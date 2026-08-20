package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
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
	Register(context.Context, string, central.RegisterProbeRequest) (central.RegisterProbeResponse, error)
	HeartbeatProbe(context.Context, Session, central.ProbeHeartbeatRequest) (central.ProbeHeartbeatResponse, error)
	DisconnectProbe(context.Context, Session) error
	ClaimAssignment(context.Context, Session) (*central.ClaimAssignmentResponse, error)
	HeartbeatAssignment(
		context.Context,
		Session,
		central.ClaimAssignmentResponse,
	) (central.AssignmentHeartbeatResponse, error)
	CommitResult(
		context.Context,
		Session,
		central.ClaimAssignmentResponse,
		central.AssignmentResult,
	) (central.ResultReceipt, error)
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
	input central.RegisterProbeRequest,
) (central.RegisterProbeResponse, error) {
	var output central.RegisterProbeResponse
	err := api.request(
		ctx, http.MethodPost, "/v1/probes/register", credential, input.InstallationID, "", input, &output,
	)
	return output, err
}

func (api *HTTPAPI) HeartbeatProbe(
	ctx context.Context,
	session Session,
	input central.ProbeHeartbeatRequest,
) (central.ProbeHeartbeatResponse, error) {
	var output central.ProbeHeartbeatResponse
	err := api.request(
		ctx, http.MethodPut, "/v1/probes/"+url.PathEscape(session.ProbeID)+"/heartbeat",
		session.AccessToken, "", "", input, &output,
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
) (*central.ClaimAssignmentResponse, error) {
	var output central.ClaimAssignmentResponse
	err := api.request(
		ctx, http.MethodPost, "/v1/probes/"+url.PathEscape(session.ProbeID)+"/assignments:claim",
		session.AccessToken, "", "", nil, &output,
	)
	if errors.Is(err, errNoContent) {
		return nil, nil
	}
	return &output, err
}

func (api *HTTPAPI) HeartbeatAssignment(
	ctx context.Context,
	session Session,
	assignment central.ClaimAssignmentResponse,
) (central.AssignmentHeartbeatResponse, error) {
	var output central.AssignmentHeartbeatResponse
	err := api.request(
		ctx, http.MethodPut, "/v1/assignments/"+url.PathEscape(assignment.AssignmentID)+"/heartbeat",
		session.AccessToken, "", assignment.LeaseToken, nil, &output,
	)
	return output, err
}

func (api *HTTPAPI) CommitResult(
	ctx context.Context,
	session Session,
	assignment central.ClaimAssignmentResponse,
	result central.AssignmentResult,
) (central.ResultReceipt, error) {
	var output central.ResultReceipt
	err := api.request(
		ctx, http.MethodPut, "/v1/assignments/"+url.PathEscape(assignment.AssignmentID)+"/result",
		session.AccessToken, result.RunID, assignment.LeaseToken, result, &output,
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
	input any,
	output any,
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
	if err := decodeJSON(contents, output); err != nil {
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
	input any,
) (*http.Request, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
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
	request.Header.Set("X-Cineko-Protocol", strconv.Itoa(central.ProtocolVersion))
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
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			RequestID string `json:"requestId"`
		} `json:"error"`
	}
	if err := decodeJSON(contents, &envelope); err != nil || strings.TrimSpace(envelope.Error.Code) == "" {
		return fmt.Errorf("central API returned HTTP %d with an invalid error envelope", response.StatusCode)
	}
	apiError := &APIError{
		StatusCode: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message,
		Retryable: envelope.Error.Retryable, RequestID: envelope.Error.RequestID,
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

func decodeJSON(contents []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("response must contain one JSON value")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
