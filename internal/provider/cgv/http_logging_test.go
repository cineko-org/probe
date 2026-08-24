package cgv

import (
	"errors"
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
