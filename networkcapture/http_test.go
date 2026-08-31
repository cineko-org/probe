package networkcapture

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestHTTPTransportAcceptsNilRequestHeaders(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := HTTPTransport(store, "fixture", nil, roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get(requestIDHeader) == "" {
			t.Fatal("request id header was not injected")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
	}))
	response, err := transport.RoundTrip(&http.Request{Method: http.MethodGet, URL: mustURL(t, "http://fixture.test"), Header: nil})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestHTTPClientCapturesComplete429Exchange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":"limited"}`)
	}))
	defer server.Close()
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := HTTPClient(store, "launcher", nil, server.Client())
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/runtime.json?platform=test", strings.NewReader(`{"version":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
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
	entries, err := List(store.Root(), Query{Status: 429})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	exchange, err := ReadExchange(store.Root(), entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if exchange.Request.URL != request.URL.String() || exchange.Response == nil || firstHeader(exchange.Response.Headers, "Retry-After") != "60" {
		t.Fatalf("exchange = %+v", exchange)
	}
	path, _, err := BodyPath(store.Root(), exchange, "response")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path) // #nosec G304 -- BodyPath validates the application-owned capture root.
	if err != nil || string(contents) != `{"error":"limited"}` {
		t.Fatalf("response body = %q, %v", contents, err)
	}
}
