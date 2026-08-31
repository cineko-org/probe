package cgv

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestExpectedBrowserRequestOutcome(t *testing.T) {
	if got := expectedBrowserRequestOutcome(errors.New("net::ERR_BLOCKED_BY_CLIENT.Inspector")); got != "blocked" {
		t.Fatalf("intentional Chromium resource block outcome = %q", got)
	}
	if got := expectedBrowserRequestOutcome(errors.New("net::ERR_ABORTED")); got != "canceled" {
		t.Fatalf("navigation cancellation outcome = %q", got)
	}
	for _, err := range []error{nil, errors.New("net::ERR_CONNECTION_RESET"), errors.New("HTTP 503")} {
		if got := expectedBrowserRequestOutcome(err); got != "" {
			t.Fatalf("unexpected network failure outcome = %q for %v", got, err)
		}
	}
}

func TestBrowserRequestLogsKeepRoutineOutcomesAtDebug(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter := &Adapter{logger: logger}

	adapter.writeBrowserRequestLog([]any{"event", "browser.network.request.completed"}, nil)
	adapter.writeBrowserRequestLog([]any{"event", "browser.network.request.completed"}, errors.New("net::ERR_BLOCKED_BY_CLIENT.Inspector"))
	adapter.writeBrowserRequestLog([]any{"event", "browser.network.request.completed"}, errors.New("HTTP 429"))

	decoder := json.NewDecoder(&output)
	for index, expectedLevel := range []string{"DEBUG", "DEBUG", "ERROR"} {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode log record %d: %v", index, err)
		}
		if record["level"] != expectedLevel {
			t.Fatalf("log record %d level = %v, want %s", index, record["level"], expectedLevel)
		}
	}
	var extra map[string]any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("unexpected extra browser network log record: %v", err)
	}
}
