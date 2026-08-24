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
	for _, expected := range []string{
		`"camel_case":1`, `"error":"secret detail"`, `"access_token":"hidden"`,
	} {
		if !strings.Contains(line, expected) {
			t.Fatalf("log %q does not contain %q", line, expected)
		}
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

func TestCanonicalLoggerPreservesDiagnosticDetails(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	raw := `GET https://alice:hunter2@example.invalid/path?access_token=query-secret ` +
		`Authorization: Bearer bearer-secret Cookie: session_id=cookie-secret ` +
		`"refresh_token":"json-secret" password=pass-secret ./relative?api_key=relative-secret ` +
		`alice:hunter2@proxy.invalid eyJabc.def.ghi`
	var output bytes.Buffer
	setup, err := New(context.Background(), "test-service", &output)
	if err != nil {
		t.Fatal(err)
	}
	setup.Logger.Error("provider failure", "error", errors.New(raw), "requestURL", "https://example.invalid/path")
	line := output.String()
	for _, detail := range []string{"hunter2", "query-secret", "bearer-secret", "json-secret", "request_url"} {
		if !strings.Contains(line, detail) {
			t.Fatalf("structured log dropped %q: %q", detail, line)
		}
	}
}
