package telemetry

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RequestIDHeader is the correlation header used at every HTTP boundary.
const RequestIDHeader = "X-Request-Id"

const defaultHTTPService = "probe"

type requestIDContextKey struct{}

var requestIDSequence atomic.Uint64

// WithRequestID returns a context carrying the request correlation ID.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestIDContextKey{}, normalizeRequestID(requestID))
}

// RequestID returns the request correlation ID carried by ctx, if any.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return normalizeRequestID(requestID)
}

// NewRequestID creates a bounded, opaque request ID. The fallback sequence is
// only used if the operating system's CSPRNG is unavailable.
func NewRequestID() string {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return "req_" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("req_%x", requestIDSequence.Add(1))
}

// HTTPServerMiddleware logs one completion event for every request handled by
// next and makes X-Request-Id available to downstream handlers.
func HTTPServerMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	logger = normalizeHTTPLogger(logger)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := requestIDFromRequest(request)
		if requestID == "" {
			requestID = NewRequestID()
		}
		request = request.WithContext(WithRequestID(request.Context(), requestID))
		request.Header = request.Header.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		request.Header.Set(RequestIDHeader, requestID)
		writer.Header().Set(RequestIDHeader, requestID)

		var requestBody *countingReadCloser
		if request.Body != nil {
			requestBody = &countingReadCloser{ReadCloser: request.Body}
			request.Body = requestBody
		}
		response := &countingResponseWriter{ResponseWriter: writer}
		started := time.Now()
		var panicValue any
		func() {
			defer func() { panicValue = recover() }()
			next.ServeHTTP(response, request)
		}()
		if panicValue != nil {
			logHTTPServerCompletion(logger, request, response, requestBody, started,
				fmt.Errorf("HTTP handler panic: %v", panicValue))
			panic(panicValue)
		}
		logHTTPServerCompletion(logger, request, response, requestBody, started, response.writeErr)
	})
}

// HTTPClientTransport wraps a RoundTripper with request ID propagation and a
// completion log. Response-body accounting is deferred until the caller
// consumes or closes the body, which keeps byte counts accurate without
// buffering arbitrary response payloads.
func HTTPClientTransport(logger *slog.Logger, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if _, alreadyWrapped := base.(*loggingTransport); alreadyWrapped {
		return base
	}
	return &loggingTransport{logger: normalizeHTTPLogger(logger), base: base}
}

// HTTPClient returns a shallow client copy whose transport is instrumented.
// It preserves timeout, redirect, cookie, and jar settings supplied by the
// caller while avoiding per-call logging at individual API methods.
func HTTPClient(logger *slog.Logger, client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.Transport = HTTPClientTransport(logger, client.Transport)
	return &copy
}

type loggingTransport struct {
	logger *slog.Logger
	base   http.RoundTripper
}

func (transport *loggingTransport) CloseIdleConnections() {
	if closer, ok := transport.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (transport *loggingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		err := errors.New("HTTP client received a nil request")
		transport.logger.Error("HTTP client request completed",
			"service", defaultHTTPService, "event", "http.client.request.completed",
			"request_id", NewRequestID(), "method", "", "route", "/", "path", "/",
			"status", 0, "duration_ms", 0, "request_bytes", 0, "response_bytes", 0,
			"outcome", "failed", "error", err)
		return nil, err
	}
	requestID := normalizeRequestID(request.Header.Get(RequestIDHeader))
	if requestID == "" {
		requestID = RequestID(request.Context())
	}
	if requestID == "" {
		requestID = NewRequestID()
	}
	requestContext := WithRequestID(request.Context(), requestID)
	request = request.Clone(requestContext)
	request.Header = request.Header.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set(RequestIDHeader, requestID)

	started := time.Now()
	var requestBody *countingReadCloser
	if request.Body != nil {
		requestBody = &countingReadCloser{ReadCloser: request.Body}
		request.Body = requestBody
	}
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		transport.logClient(request, requestBody, nil, nil, started, err)
		return nil, err
	}
	if response == nil {
		err = errors.New("HTTP RoundTripper returned a nil response")
		transport.logClient(request, requestBody, nil, nil, started, err)
		return nil, err
	}
	if response.Body == nil {
		transport.logClient(request, requestBody, response, nil, started, nil)
		return response, nil
	}
	response.Body = &loggingResponseBody{
		ReadCloser:  response.Body,
		transport:   transport,
		request:     request,
		requestBody: requestBody,
		response:    response,
		started:     started,
	}
	return response, nil
}

func (transport *loggingTransport) logClient(
	request *http.Request,
	requestBody *countingReadCloser,
	response *http.Response,
	responseBody *loggingResponseBody,
	started time.Time,
	rawErr error,
) {
	attrs := []any{
		"service", defaultHTTPService,
		"event", "http.client.request.completed",
		"request_id", requestIDFromRequest(request),
		"method", request.Method,
		"route", requestPath(request.URL),
		"path", requestPath(request.URL),
		"duration_ms", time.Since(started).Milliseconds(),
	}
	requestBytes := countedRequestBytes(request, requestBody)
	if requestBytes < 0 {
		requestBytes = 0
	}
	attrs = append(attrs, "request_bytes", requestBytes)
	status := 0
	responseBytes := int64(0)
	if response != nil {
		status = response.StatusCode
		if measuredBytes := responseBodyBytes(response, responseBody); measuredBytes >= 0 {
			responseBytes = measuredBytes
		}
	}
	attrs = append(attrs, "status", status, "response_bytes", responseBytes)
	if rawErr == nil && status >= http.StatusBadRequest {
		rawErr = fmt.Errorf("HTTP %d", status)
	}
	if rawErr != nil {
		attrs = append(attrs, "outcome", "failed", "error", rawErr)
		transport.logger.Error("HTTP client request completed", attrs...)
		return
	}
	attrs = append(attrs, "outcome", "succeeded")
	transport.logger.Info("HTTP client request completed", attrs...)
}

type loggingResponseBody struct {
	io.ReadCloser
	transport   *loggingTransport
	request     *http.Request
	requestBody *countingReadCloser
	response    *http.Response
	started     time.Time
	readMu      sync.Mutex
	readErr     error
	bytes       atomic.Int64
	finishOnce  sync.Once
}

func (body *loggingResponseBody) Read(buffer []byte) (int, error) {
	read, err := body.ReadCloser.Read(buffer)
	if read > 0 {
		body.bytes.Add(int64(read))
	}
	if err != nil && !errors.Is(err, io.EOF) {
		body.readMu.Lock()
		body.readErr = err
		body.readMu.Unlock()
	}
	if errors.Is(err, io.EOF) {
		body.finish(nil)
	}
	return read, err
}

func (body *loggingResponseBody) Close() error {
	err := body.ReadCloser.Close()
	if err != nil {
		body.readMu.Lock()
		body.readErr = err
		body.readMu.Unlock()
	}
	body.finish(err)
	return err
}

func (body *loggingResponseBody) finish(closeErr error) {
	body.finishOnce.Do(func() {
		body.readMu.Lock()
		rawErr := body.readErr
		body.readMu.Unlock()
		if rawErr == nil {
			rawErr = closeErr
		}
		body.transport.logClient(body.request, body.requestBody, body.response, body, body.started, rawErr)
	})
}

type countingReadCloser struct {
	io.ReadCloser
	bytes atomic.Int64
}

func (body *countingReadCloser) Read(buffer []byte) (int, error) {
	read, err := body.ReadCloser.Read(buffer)
	if read > 0 {
		body.bytes.Add(int64(read))
	}
	return read, err
}

func countedRequestBytes(request *http.Request, body *countingReadCloser) int64 {
	if body != nil {
		if bytes := body.bytes.Load(); bytes > 0 || request.ContentLength == 0 {
			return bytes
		}
		if request.ContentLength > 0 {
			return request.ContentLength
		}
		return -1
	}
	if request.ContentLength >= 0 {
		return request.ContentLength
	}
	return -1
}

func responseBodyBytes(response *http.Response, body *loggingResponseBody) int64 {
	if body != nil {
		if bytes := body.bytes.Load(); bytes > 0 || response.ContentLength == 0 {
			return bytes
		}
		if response.ContentLength > 0 {
			return response.ContentLength
		}
		return -1
	}
	if response.ContentLength >= 0 {
		return response.ContentLength
	}
	return -1
}

type countingResponseWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	wrote    bool
	writeErr error
}

func (writer *countingResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		writer.ResponseWriter.WriteHeader(status)
		return
	}
	if writer.wrote {
		return
	}
	writer.status = status
	writer.wrote = true
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *countingResponseWriter) Write(buffer []byte) (int, error) {
	if !writer.wrote {
		writer.WriteHeader(http.StatusOK)
	}
	written, err := writer.ResponseWriter.Write(buffer)
	writer.bytes += int64(written)
	if err != nil {
		writer.writeErr = err
	}
	return written, err
}

func (writer *countingResponseWriter) ReadFrom(source io.Reader) (int64, error) {
	if !writer.wrote {
		writer.WriteHeader(http.StatusOK)
	}
	if reader, ok := writer.ResponseWriter.(io.ReaderFrom); ok {
		written, err := reader.ReadFrom(source)
		writer.bytes += written
		if err != nil {
			writer.writeErr = err
		}
		return written, err
	}
	written, err := io.Copy(writer, source)
	return written, err
}

func (writer *countingResponseWriter) Flush() {
	if !writer.wrote {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *countingResponseWriter) Hijack() (netConn net.Conn, readWriteBuf *bufio.ReadWriter, err error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("HTTP response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (writer *countingResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (writer *countingResponseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func logHTTPServerCompletion(
	logger *slog.Logger,
	request *http.Request,
	response *countingResponseWriter,
	requestBody *countingReadCloser,
	started time.Time,
	rawErr error,
) {
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	attrs := []any{
		"service", defaultHTTPService,
		"event", "http.server.request.completed",
		"request_id", requestIDFromRequest(request),
		"method", request.Method,
		"route", serverRoute(request),
		"path", requestPath(request.URL),
		"status", status,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	requestBytes := countedRequestBytes(request, requestBody)
	if requestBytes < 0 {
		requestBytes = 0
	}
	attrs = append(attrs, "request_bytes", requestBytes, "response_bytes", response.bytes)
	if rawErr == nil && status >= http.StatusBadRequest {
		rawErr = fmt.Errorf("HTTP %d", status)
	}
	if rawErr != nil {
		attrs = append(attrs, "outcome", "failed", "error", rawErr)
		logger.Error("HTTP server request completed", attrs...)
		return
	}
	attrs = append(attrs, "outcome", "succeeded")
	logger.Info("HTTP server request completed", attrs...)
}

func normalizeHTTPLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}

func requestIDFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	if requestID := RequestID(request.Context()); requestID != "" {
		return requestID
	}
	return normalizeRequestID(request.Header.Get(RequestIDHeader))
}

func normalizeRequestID(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 128 || strings.ContainsAny(requestID, "\r\n") {
		return ""
	}
	return requestID
}

func requestPath(requestURL *url.URL) string {
	if requestURL == nil || requestURL.Path == "" {
		return "/"
	}
	return requestURL.Path
}

func serverRoute(request *http.Request) string {
	if request != nil && strings.TrimSpace(request.Pattern) != "" {
		return request.Pattern
	}
	if request == nil {
		return "/"
	}
	return requestPath(request.URL)
}
