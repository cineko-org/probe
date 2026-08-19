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
