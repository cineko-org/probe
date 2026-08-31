package networkcapture

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestRateLimitGateHonorsRetryAfterAndAllowsOneHalfOpenRequest(t *testing.T) {
	now := time.Date(2026, time.August, 25, 1, 34, 8, 0, time.UTC)
	gate := NewRateLimitGate()
	gate.now = func() time.Time { return now }
	gate.jitter = func(time.Duration) time.Duration { return 0 }
	decision := gate.Observe429("CGV.CO.KR", []Header{{Name: "Retry-After", Value: "60"}})
	if decision.Source != "retry-after" || decision.Delay != time.Minute {
		t.Fatalf("decision = %+v", decision)
	}
	duplicate := gate.Observe429("cgv.co.kr", nil)
	if duplicate.Failures != 1 || duplicate.BlockedUntil != decision.BlockedUntil {
		t.Fatalf("same-burst 429 escalated the circuit: %+v", duplicate)
	}
	if allowed, _ := gate.Allow("cgv.co.kr"); allowed {
		t.Fatal("request allowed during Retry-After")
	}
	now = now.Add(time.Minute)
	if allowed, _ := gate.Allow("cgv.co.kr"); !allowed {
		t.Fatal("first half-open request was blocked")
	}
	if allowed, _ := gate.Allow("cgv.co.kr"); allowed {
		t.Fatal("second half-open request was allowed")
	}
	if !gate.ObserveSuccess("cgv.co.kr") {
		t.Fatal("half-open success did not close circuit")
	}
	if allowed, _ := gate.Allow("cgv.co.kr"); !allowed {
		t.Fatal("request blocked after successful half-open request")
	}
}

func TestRateLimitGateUsesBoundedExponentialFallback(t *testing.T) {
	now := time.Date(2026, time.August, 25, 1, 34, 8, 0, time.UTC)
	gate := NewRateLimitGate()
	gate.now = func() time.Time { return now }
	gate.jitter = func(time.Duration) time.Duration { return 0 }
	first := gate.Observe429("cgv.co.kr", nil)
	if first.Source != "exponential_fallback" || first.Delay != 30*time.Second {
		t.Fatalf("first fallback = %+v", first)
	}
	now = first.BlockedUntil
	if allowed, _ := gate.Allow("cgv.co.kr"); !allowed {
		t.Fatal("half-open probe blocked")
	}
	second := gate.Observe429("cgv.co.kr", nil)
	if second.Delay != time.Minute || second.Failures != 2 {
		t.Fatalf("second fallback = %+v", second)
	}
	for range 20 {
		now = second.BlockedUntil
		_, _ = gate.Allow("cgv.co.kr")
		second = gate.Observe429("cgv.co.kr", nil)
	}
	if second.Delay != 15*time.Minute {
		t.Fatalf("fallback cap = %s, want 15m", second.Delay)
	}
}

func TestRateLimitGateReopensWhenHalfOpenTransportFails(t *testing.T) {
	now := time.Date(2026, time.August, 25, 1, 34, 8, 0, time.UTC)
	gate := NewRateLimitGate()
	gate.now = func() time.Time { return now }
	gate.jitter = func(time.Duration) time.Duration { return 0 }
	first := gate.Observe429("cgv.co.kr", []Header{{Name: "Retry-After", Value: "1"}})
	now = first.BlockedUntil
	if allowed, _ := gate.Allow("cgv.co.kr"); !allowed {
		t.Fatal("half-open request was blocked")
	}
	decision, observed := gate.ObserveFailure("cgv.co.kr")
	if !observed || decision.Failures != 2 || decision.Delay != time.Minute {
		t.Fatalf("half-open transport decision = %+v, %t", decision, observed)
	}
	if allowed, _ := gate.Allow("cgv.co.kr"); allowed {
		t.Fatal("request allowed after failed half-open probe")
	}
}

func TestRetryDeadlineAcceptsHTTPDateAndResetHeaders(t *testing.T) {
	now := time.Date(2026, time.August, 25, 1, 34, 8, 0, time.UTC)
	deadline := now.Add(90 * time.Second).Truncate(time.Second)
	if got, source, ok := retryDeadline(now, []Header{{Name: "Retry-After", Value: deadline.Format(http.TimeFormat)}}); !ok || source != "retry-after" || !got.Equal(deadline) {
		t.Fatalf("HTTP-date deadline = %s, %s, %t", got, source, ok)
	}
	if got, source, ok := retryDeadline(now, []Header{{Name: "RateLimit-Reset", Value: "12"}}); !ok || source != "ratelimit-reset" || got.Sub(now) != 12*time.Second {
		t.Fatalf("relative reset deadline = %s, %s, %t", got, source, ok)
	}
	epoch := now.Add(3 * time.Minute).Unix()
	if got, _, ok := retryDeadline(now, []Header{{Name: "X-RateLimit-Reset", Value: "not-a-time"}}); ok || !got.IsZero() {
		t.Fatalf("non-numeric reset accepted: %s", got)
	}
	if got, source, ok := retryDeadline(now, []Header{{Name: "X-RateLimit-Reset", Value: strconv.FormatInt(epoch, 10)}}); !ok || source != "x-ratelimit-reset" || got.Unix() != epoch {
		t.Fatalf("epoch reset deadline = %s, %s, %t", got, source, ok)
	}
	nearEpoch := now.Add(30 * time.Second).Unix()
	if got, _, ok := retryDeadline(now, []Header{{Name: "X-RateLimit-Reset", Value: strconv.FormatInt(nearEpoch, 10)}}); !ok || got.Unix() != nearEpoch {
		t.Fatalf("near epoch reset deadline = %s, %t", got, ok)
	}
}
