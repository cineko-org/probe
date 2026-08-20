package cgv

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestProviderHTTPErrorClassification(t *testing.T) {
	t.Parallel()
	if err := providerHTTPError(403); !errors.Is(err, ErrProviderAccessBlocked) {
		t.Fatalf("403 error = %v", err)
	}
	if err := providerHTTPError(429); !errors.Is(err, ErrProviderThrottled) {
		t.Fatalf("429 error = %v", err)
	}
	if err := providerHTTPError(500); errors.Is(err, ErrProviderAccessBlocked) || errors.Is(err, ErrProviderThrottled) {
		t.Fatalf("500 error = %v", err)
	}
}

func TestCapturedProviderFailureUsesConfiguredHandler(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	adapter := &Adapter{
		ctx: context.Background(),
		providerResponses: []capturedProviderResponse{{
			path: scheduleResponsePath,
			err:  providerHTTPError(403),
		}},
		providerFailureHandler: func(_ context.Context, err error) error {
			calls.Add(1)
			if !errors.Is(err, ErrProviderAccessBlocked) {
				t.Fatalf("provider error = %v", err)
			}
			return nil
		},
	}
	if _, err := adapter.captureScheduleRows(); !errors.Is(err, ErrProviderAccessBlocked) {
		t.Fatalf("captureScheduleRows() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
}
