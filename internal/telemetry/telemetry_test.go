package telemetry

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewFallsBackToCanonicalJSONWithoutOTLP(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	var output bytes.Buffer
	setup, err := New(context.Background(), "test-service", &output)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	setup.Logger.Error("failed", "camelCase", 1, "error", errors.New("secret detail"), "access_token", "hidden")
	line := output.String()
	for _, expected := range []string{`"camel_case":1`, `"error_type":"error_string"`} {
		if !strings.Contains(line, expected) {
			t.Fatalf("log %q does not contain %q", line, expected)
		}
	}
	if strings.Contains(line, "secret detail") || strings.Contains(line, "hidden") {
		t.Fatalf("log contains sensitive value: %q", line)
	}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestErrorType(t *testing.T) {
	if got := ErrorType(nil); got != "" {
		t.Fatalf("ErrorType(nil) = %q", got)
	}
	if got := ErrorType(errors.New("detail")); got != "error_string" {
		t.Fatalf("ErrorType(error) = %q", got)
	}
}

func TestSafeDiagnosticBoundsAndNormalizesProviderText(t *testing.T) {
	if SafeDiagnostic(nil) != "" {
		t.Fatal("SafeDiagnostic(nil) was not empty")
	}
	value := SafeDiagnostic(errors.New("provider\nresponse\tfailed\x00"))
	if value != "error_string" {
		t.Fatalf("SafeDiagnostic() = %q", value)
	}
	if got := SafeDiagnostic(errors.New(strings.Repeat("x", 1025))); got != "error_string" {
		t.Fatalf("SafeDiagnostic() = %q", got)
	}
	if got := SafeDiagnostic(explodingDiagnosticError{}); got != "exploding_diagnostic_error" {
		t.Fatalf("SafeDiagnostic() read error text or returned the wrong class: %q", got)
	}
}

type explodingDiagnosticError struct{}

func (explodingDiagnosticError) Error() string { panic("SafeDiagnostic must not read error text") }

func TestSafeDiagnosticRedactsProviderSecretsAndUsesSafeLogKey(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	raw := `GET https://alice:hunter2@example.invalid/path?access_token=query-secret ` +
		`Authorization: Bearer bearer-secret Cookie: session_id=cookie-secret ` +
		`"refresh_token":"json-secret" password=pass-secret ./relative?api_key=relative-secret ` +
		`alice:hunter2@proxy.invalid eyJabc.def.ghi`
	sanitized := SafeDiagnostic(errors.New(raw))
	for _, forbidden := range []string{
		"hunter2", "query-secret", "bearer-secret", "cookie-secret", "json-secret",
		"pass-secret", "relative-secret", "eyJabc", "https://", "./relative?",
	} {
		if strings.Contains(sanitized, forbidden) {
			t.Fatalf("SafeDiagnostic() leaked %q in %q", forbidden, sanitized)
		}
	}
	if sanitized != "error_string" {
		t.Fatalf("SafeDiagnostic() exposed provider-controlled text: %q", sanitized)
	}

	var output bytes.Buffer
	setup, err := New(context.Background(), "test-service", &output)
	if err != nil {
		t.Fatal(err)
	}
	setup.Logger.Error("provider failure", SafeDiagnosticKey, sanitized)
	line := output.String()
	if !strings.Contains(line, `"provider_error_summary":`) {
		t.Fatalf("sanitized diagnostic key was dropped: %q", line)
	}
	for _, forbidden := range []string{"hunter2", "bearer-secret", "json-secret"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("structured log leaked %q: %q", forbidden, line)
		}
	}
}
