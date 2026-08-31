package networkcapture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const requestIDHeader = "X-Request-Id"

type httpTransport struct {
	store   *Store
	service string
	logger  *slog.Logger
	base    http.RoundTripper
}

func HTTPClient(store *Store, service string, logger *slog.Logger, client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.Transport = HTTPTransport(store, service, logger, client.Transport)
	return &copy
}

func HTTPTransport(store *Store, service string, logger *slog.Logger, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if existing, ok := base.(*httpTransport); ok && existing.store == store {
		return base
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &httpTransport{store: store, service: strings.TrimSpace(service), logger: logger, base: base}
}

func (transport *httpTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("network capture HTTP transport received a nil request")
	}
	requestID := strings.TrimSpace(request.Header.Get(requestIDHeader))
	if requestID == "" {
		requestID = newID(time.Now())
	}
	request = request.Clone(context.WithValue(request.Context(), httpCaptureContextKey{}, requestID))
	request.Header = request.Header.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set(requestIDHeader, requestID)
	started := time.Now()
	requestStage := httpStage{}
	if transport.store != nil && transport.store.DebugEnabled() {
		requestStage = openHTTPStage(transport.store)
	}
	if request.Body != nil && requestStage.file != nil {
		request.Body = &httpTeeReadCloser{Reader: io.TeeReader(request.Body, requestStage.file), Closer: request.Body}
	}
	response, err := transport.base.RoundTrip(request)
	requestStage.close()
	if err != nil || response == nil {
		if err == nil {
			err = errors.New("HTTP RoundTripper returned a nil response")
		}
		transport.save(request, response, started, requestStage, httpStage{}, err)
		requestStage.cleanup()
		return response, err
	}
	if response.Body == nil {
		transport.save(request, response, started, requestStage, httpStage{}, nil)
		requestStage.cleanup()
		return response, nil
	}
	responseStage := httpStage{}
	if transport.store != nil && (transport.store.DebugEnabled() || response.StatusCode >= http.StatusBadRequest) {
		responseStage = openHTTPStage(transport.store)
	}
	response.Body = &capturingHTTPBody{
		ReadCloser: response.Body, transport: transport, request: request, response: response,
		started: started, requestStage: requestStage, responseStage: responseStage,
	}
	return response, nil
}

type httpCaptureContextKey struct{}

type httpTeeReadCloser struct {
	io.Reader
	io.Closer
}

type httpStage struct {
	file       io.WriteCloser
	path       string
	cleanupFn  func()
	writeError error
}

func openHTTPStage(store *Store) httpStage {
	if store == nil {
		return httpStage{}
	}
	file, cleanup, err := store.NewStagingFile()
	if err != nil {
		return httpStage{writeError: err}
	}
	return httpStage{file: file, path: file.Name(), cleanupFn: cleanup}
}

func (stage *httpStage) close() {
	if stage == nil || stage.file == nil {
		return
	}
	stage.writeError = errors.Join(stage.writeError, stage.file.Close())
	stage.file = nil
}

func (stage httpStage) cleanup() {
	if stage.cleanupFn != nil {
		stage.cleanupFn()
	}
}

type capturingHTTPBody struct {
	io.ReadCloser
	transport     *httpTransport
	request       *http.Request
	response      *http.Response
	started       time.Time
	requestStage  httpStage
	responseStage httpStage
	readError     error
	once          sync.Once
}

func (body *capturingHTTPBody) Read(contents []byte) (int, error) {
	count, err := body.ReadCloser.Read(contents)
	if count > 0 && body.responseStage.file != nil && body.responseStage.writeError == nil {
		_, body.responseStage.writeError = body.responseStage.file.Write(contents[:count])
	}
	if err != nil {
		if err != io.EOF {
			body.readError = err
		}
		body.complete()
	}
	return count, err
}

func (body *capturingHTTPBody) Close() error {
	if body.readError == nil {
		_, body.readError = io.Copy(io.Discard, body)
	}
	err := body.ReadCloser.Close()
	body.readError = errors.Join(body.readError, err)
	body.complete()
	return err
}

func (body *capturingHTTPBody) complete() {
	body.once.Do(func() {
		body.requestStage.close()
		body.responseStage.close()
		body.transport.save(body.request, body.response, body.started, body.requestStage, body.responseStage, body.readError)
		body.requestStage.cleanup()
		body.responseStage.cleanup()
	})
}

func (transport *httpTransport) save(
	request *http.Request,
	response *http.Response,
	started time.Time,
	requestStage httpStage,
	responseStage httpStage,
	rawErr error,
) {
	if transport == nil || transport.store == nil || request == nil {
		return
	}
	if !transport.store.DebugEnabled() && rawErr == nil && response != nil && response.StatusCode < http.StatusBadRequest {
		return
	}
	captureErr := errors.Join(rawErr, requestStage.writeError, responseStage.writeError)
	if response != nil && response.StatusCode >= http.StatusBadRequest {
		captureErr = errors.Join(captureErr, fmt.Errorf("HTTP %d", response.StatusCode))
	}
	outcome, errorText := "succeeded", ""
	if captureErr != nil {
		outcome, errorText = "failed", captureErr.Error()
	}
	requestID, _ := request.Context().Value(httpCaptureContextKey{}).(string)
	record := Record{
		Exchange: Exchange{
			CorrelationID: requestID, Service: transport.service, Scenario: "http_client", Transport: "go_http",
			StartedAt: started, CompletedAt: time.Now(), Outcome: outcome, Error: errorText,
			Request: Request{Method: request.Method, URL: request.URL.String(), Headers: standardHTTPHeaders(request.Header), Bytes: max(request.ContentLength, 0)},
		},
		RequestBodyPath: requestStage.path, RequestContentType: request.Header.Get("Content-Type"), RequestRepresentation: "application",
		ResponseBodyPath: responseStage.path, ResponseRepresentation: "application",
	}
	if response != nil {
		record.Response = &Response{
			Status: response.StatusCode, StatusText: http.StatusText(response.StatusCode), Protocol: response.Proto,
			Headers: standardHTTPHeaders(response.Header), Bytes: max(response.ContentLength, 0),
		}
		record.ResponseContentType = response.Header.Get("Content-Type")
		if response.Uncompressed {
			record.ResponseRepresentation = "decoded"
		} else {
			record.ResponseRepresentation = "encoded"
		}
	}
	if _, err := transport.store.Save(context.WithoutCancel(request.Context()), record); err != nil {
		transport.logger.ErrorContext(request.Context(), "Network HTTP exchange capture failed",
			"event", "network.capture.save.failed", "request_id", requestID,
			"method", request.Method, "request_url", request.URL.String(), "error", err)
	}
}

func standardHTTPHeaders(headers http.Header) []Header {
	result := make([]Header, 0, len(headers))
	for name, values := range headers {
		for _, value := range values {
			result = append(result, Header{Name: name, Value: value})
		}
	}
	return result
}
