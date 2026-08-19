package main

import (
	"context"
	"strings"
	"testing"
)

func TestPreflightEgressAllowsUnconfiguredDevelopmentMode(t *testing.T) {
	clearEgressEnvironment(t)
	if err := preflightEgress(context.Background()); err != nil {
		t.Fatalf("preflightEgress() error = %v", err)
	}
}

func TestPreflightEgressRejectsIncompleteSoxyConfiguration(t *testing.T) {
	clearEgressEnvironment(t)
	t.Setenv("CINEKO_SOXY_URL", "https://soxy.example.test")
	if err := preflightEgress(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "configure Soxy URL and API token together") {
		t.Fatalf("preflightEgress() error = %v", err)
	}
}

func TestPreflightEgressRejectsPlaintextToken(t *testing.T) {
	clearEgressEnvironment(t)
	t.Setenv("CINEKO_SOXY_API_TOKEN", "must-not-be-logged")
	if err := preflightEgress(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "CINEKO_SOXY_API_TOKEN is not supported") ||
		strings.Contains(err.Error(), "must-not-be-logged") {
		t.Fatalf("preflightEgress() error = %v", err)
	}
}

func TestPreflightEgressRequiresConfiguredProxyInProduction(t *testing.T) {
	clearEgressEnvironment(t)
	t.Setenv("CINEKO_REQUIRE_PROXY", "true")
	if err := preflightEgress(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no proxy is configured") {
		t.Fatalf("preflightEgress() error = %v", err)
	}
}

func TestPreflightEgressRejectsInvalidProxyRequirement(t *testing.T) {
	clearEgressEnvironment(t)
	t.Setenv("CINEKO_REQUIRE_PROXY", "sometimes")
	if err := preflightEgress(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("preflightEgress() error = %v", err)
	}
}

func clearEgressEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CINEKO_REQUIRE_PROXY",
		"CINEKO_SOXY_URL",
		"CINEKO_SOXY_API_TOKEN",
		"CINEKO_SOXY_API_TOKEN_FILE",
		"CINEKO_SOXY_SESSION_TTL",
		"CINEKO_SCAN_PROXIES",
		"CINEKO_SCAN_PROXIES_FILE",
	} {
		t.Setenv(name, "")
	}
}
