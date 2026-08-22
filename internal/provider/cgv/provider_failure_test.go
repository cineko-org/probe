package cgv

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestProviderHTTPErrorClassification(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status int
		want   error
	}{
		{status: 301, want: ErrUIContractChanged},
		{status: 400, want: ErrProviderInvalidResult},
		{status: 401, want: ErrAuthenticationRequired},
		{status: 403, want: ErrProviderAccessBlocked},
		{status: 404, want: ErrUIContractChanged},
		{status: 408, want: context.DeadlineExceeded},
		{status: 422, want: ErrProviderInvalidResult},
		{status: 429, want: ErrProviderThrottled},
		{status: 500, want: ErrProviderServerError},
		{status: 504, want: context.DeadlineExceeded},
		{status: 0, want: ErrProviderTransport},
	} {
		if err := providerHTTPError(test.status); !errors.Is(err, test.want) {
			t.Errorf("HTTP %d error = %v, want %v", test.status, err, test.want)
		}
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
