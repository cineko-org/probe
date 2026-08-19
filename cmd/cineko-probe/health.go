package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	probeHealthListen = "127.0.0.1:8081"
	probeReadyURL     = "http://127.0.0.1:8081/readyz"
)

type healthServer struct {
	ready  atomic.Bool
	server *http.Server
	errors chan error
}

func startHealthServer(address string) (*healthServer, error) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for Probe health checks: %w", err)
	}
	health := &healthServer{errors: make(chan error, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.handleLive)
	mux.HandleFunc("GET /readyz", health.handleReady)
	health.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	go func() {
		err := health.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		health.errors <- err
		close(health.errors)
	}()
	return health, nil
}

func (health *healthServer) handleLive(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, "ok\n")
}

func (health *healthServer) handleReady(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !health.ready.Load() {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, "not ready\n")
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, "ready\n")
}

func (health *healthServer) setReady(ready bool) {
	health.ready.Store(ready)
}

func (health *healthServer) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return health.server.Shutdown(ctx)
}

func checkProbeReadiness(ctx context.Context, rawURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("request Probe readiness: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("probe readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}
