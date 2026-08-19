package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandlersExposeOnlyProcessState(t *testing.T) {
	health := &healthServer{}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	health.handleLive(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("liveness = %d %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	response = httptest.NewRecorder()
	health.handleReady(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "not ready\n" {
		t.Fatalf("initial readiness = %d %q", response.Code, response.Body.String())
	}

	health.setReady(true)
	response = httptest.NewRecorder()
	health.handleReady(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "ready\n" {
		t.Fatalf("ready response = %d %q", response.Code, response.Body.String())
	}
}

func TestCheckProbeReadinessRequiresHTTP200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.URL.RawQuery != "" {
			t.Fatal("health check sent credentials or query data")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := checkProbeReadiness(context.Background(), server.URL); err == nil {
		t.Fatal("non-200 readiness accepted")
	}

	readyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer readyServer.Close()
	if err := checkProbeReadiness(context.Background(), readyServer.URL); err != nil {
		t.Fatal(err)
	}
	if err := checkProbeReadiness(context.Background(), "://invalid"); err == nil {
		t.Fatal("invalid readiness URL accepted")
	}
}

func TestHealthServerLifecycle(t *testing.T) {
	health, err := startHealthServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := health.close(); err != nil {
		t.Fatal(err)
	}
	if err := <-health.errors; err != nil {
		t.Fatal(err)
	}
}
