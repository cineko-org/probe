package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/cineko-org/probe/v2/internal/adapters/cgv"
	"github.com/cineko-org/probe/v2/internal/egress"
	"github.com/cineko-org/probe/v2/internal/telemetry"
	"github.com/cineko-org/probe/v2/probe"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := checkProbeReadiness(ctx, probeReadyURL)
		cancel()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "cineko-probe healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "browser-preflight" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := cgv.PreflightBrowserRuntime(ctx, os.Getenv("CINEKO_CHROME_PATH"))
		cancel()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "cineko-probe browser preflight: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "cineko-probe: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	config, err := loadConfig(os.Stdin)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := preflightEgress(ctx); err != nil {
		return err
	}
	telemetrySetup, err := telemetry.New(ctx, "cineko-probe", os.Stderr)
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	logger := telemetrySetup.Logger
	defer shutdownTelemetry(telemetrySetup.Shutdown)
	runtime, err := probe.NewBrowserRuntime(probe.BrowserRuntimeConfig{
		CentralURL: config.centralURL, DataDir: config.dataDir,
		HTTPClient: &http.Client{Timeout: 20 * time.Second}, Credentials: config.credentials,
		Registration: config.registration, Runtime: probe.Config{Logger: logger},
	})
	if err != nil {
		return err
	}
	defer runtime.Close()
	if config.mode != "container" {
		if err := runtime.Preflight(ctx); err != nil {
			return fmt.Errorf("browser dependency preflight: %w", err)
		}
		return runtime.Run(ctx)
	}
	health, err := startHealthServer(probeHealthListen)
	if err != nil {
		return err
	}
	defer func() { _ = health.close() }()
	if err := runtime.Preflight(ctx); err != nil {
		return fmt.Errorf("browser dependency preflight: %w", err)
	}
	return runReady(ctx, runtime, health)
}

func shutdownTelemetry(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "cineko-probe: flush telemetry: %v\n", err)
	}
}

func preflightEgress(parent context.Context) error {
	config, err := egress.ConfigFromEnvironment()
	if err != nil {
		return fmt.Errorf("load egress configuration: %w", err)
	}
	requireProxy, err := requireProxyFromEnvironment()
	if err != nil {
		return err
	}
	if requireProxy && config.SoxyURL == "" && len(config.Proxies)+len(config.ScanProxies) == 0 {
		return errors.New("CINEKO_REQUIRE_PROXY is true but no proxy is configured")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := egress.ValidateConfig(ctx, config); err != nil {
		return fmt.Errorf("egress preflight: %w", err)
	}
	return nil
}

func requireProxyFromEnvironment() (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CINEKO_REQUIRE_PROXY"))) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("CINEKO_REQUIRE_PROXY must be true or false")
	}
}

func runReady(parent context.Context, runtime *probe.BrowserRuntime, health *healthServer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- runtime.RunReady(ctx, ready) }()
	select {
	case err := <-ready:
		if err != nil {
			return <-done
		}
		health.setReady(true)
	case err := <-health.errors:
		cancel()
		<-done
		if err == nil {
			return errors.New("probe health server stopped unexpectedly")
		}
		return err
	case <-parent.Done():
		cancel()
		<-done
		return nil
	}

	select {
	case err := <-done:
		health.setReady(false)
		return err
	case err := <-health.errors:
		health.setReady(false)
		cancel()
		<-done
		if err == nil {
			return errors.New("probe health server stopped unexpectedly")
		}
		return err
	case <-parent.Done():
		health.setReady(false)
		cancel()
		<-done
		return nil
	}
}
