package networkcapture

import (
	"crypto/rand"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultRateLimitFallback = 30 * time.Second
	maximumRateLimitFallback = 15 * time.Minute
)

type RateLimitDecision struct {
	BlockedUntil time.Time
	Delay        time.Duration
	Source       string
	Failures     int
}

type rateLimitState struct {
	blockedUntil time.Time
	failures     int
	halfOpen     bool
}

// RateLimitGate is a host-scoped circuit breaker. After the first 429 it
// rejects new work until the provider deadline, then permits exactly one
// half-open request. Other callers remain blocked until that request succeeds
// or produces the next 429.
type RateLimitGate struct {
	mu       sync.Mutex
	states   map[string]*rateLimitState
	now      func() time.Time
	fallback time.Duration
	maximum  time.Duration
	jitter   func(time.Duration) time.Duration
}

func NewRateLimitGate() *RateLimitGate {
	return &RateLimitGate{
		states: make(map[string]*rateLimitState), now: time.Now,
		fallback: defaultRateLimitFallback, maximum: maximumRateLimitFallback,
		jitter: randomRateLimitJitter,
	}
}

// Allow reserves the only half-open request after a blocked period expires.
func (gate *RateLimitGate) Allow(key string) (bool, RateLimitDecision) {
	if gate == nil {
		return true, RateLimitDecision{}
	}
	key = normalizedRateLimitKey(key)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state := gate.states[key]
	if state == nil {
		return true, RateLimitDecision{}
	}
	now := gate.now()
	decision := decisionFromState(now, state, "circuit")
	if now.Before(state.blockedUntil) || state.halfOpen {
		return false, decision
	}
	state.halfOpen = true
	return true, decision
}

// Blocked reports the active deadline without reserving the half-open request.
func (gate *RateLimitGate) Blocked(key string) (bool, RateLimitDecision) {
	if gate == nil {
		return false, RateLimitDecision{}
	}
	key = normalizedRateLimitKey(key)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state := gate.states[key]
	if state == nil {
		return false, RateLimitDecision{}
	}
	now := gate.now()
	return now.Before(state.blockedUntil) || state.halfOpen, decisionFromState(now, state, "circuit")
}

// Observe429 opens the circuit using provider headers when possible and a
// bounded exponential fallback otherwise.
func (gate *RateLimitGate) Observe429(key string, headers []Header) RateLimitDecision {
	if gate == nil {
		return RateLimitDecision{}
	}
	key = normalizedRateLimitKey(key)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	now := gate.now()
	state := gate.states[key]
	if state == nil {
		state = &rateLimitState{}
		gate.states[key] = state
	}
	if now.Before(state.blockedUntil) && !state.halfOpen {
		if retryAt, source, ok := retryDeadline(now, headers); ok && retryAt.After(state.blockedUntil) {
			state.blockedUntil = retryAt
			return decisionFromState(now, state, source)
		}
		return decisionFromState(now, state, "active_circuit")
	}
	state.failures++
	retryAt, source, ok := retryDeadline(now, headers)
	if !ok || !retryAt.After(now) {
		exponent := min(state.failures-1, 20)
		delay := time.Duration(float64(gate.fallback) * math.Pow(2, float64(exponent)))
		if delay > gate.maximum || delay < 0 {
			delay = gate.maximum
		}
		jitterMaximum := min(delay/5, 5*time.Second)
		if gate.jitter != nil {
			delay = min(delay+gate.jitter(jitterMaximum), gate.maximum)
		}
		retryAt = now.Add(delay)
		source = "exponential_fallback"
	}
	state.blockedUntil = retryAt
	state.halfOpen = false
	decision := decisionFromState(now, state, source)
	return decision
}

func randomRateLimitJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)+1))
	if err != nil {
		return 0
	}
	return time.Duration(value.Int64())
}

// ObserveSuccess closes a half-open circuit. Ordinary successful traffic does
// not alter a closed circuit.
func (gate *RateLimitGate) ObserveSuccess(key string) bool {
	if gate == nil {
		return false
	}
	key = normalizedRateLimitKey(key)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state := gate.states[key]
	if state == nil || !state.halfOpen {
		return false
	}
	delete(gate.states, key)
	return true
}

// ObserveFailure reopens a half-open circuit when its single probe fails
// before any HTTP response arrives. Failures on a normally closed circuit are
// unrelated transport errors and do not create a rate-limit state.
func (gate *RateLimitGate) ObserveFailure(key string) (RateLimitDecision, bool) {
	if gate == nil {
		return RateLimitDecision{}, false
	}
	key = normalizedRateLimitKey(key)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state := gate.states[key]
	if state == nil || !state.halfOpen {
		return RateLimitDecision{}, false
	}
	now := gate.now()
	state.failures++
	delay := time.Duration(float64(gate.fallback) * math.Pow(2, float64(min(state.failures-1, 20))))
	if delay > gate.maximum || delay < 0 {
		delay = gate.maximum
	}
	if gate.jitter != nil {
		delay = min(delay+gate.jitter(min(delay/5, 5*time.Second)), gate.maximum)
	}
	state.blockedUntil = now.Add(delay)
	state.halfOpen = false
	return decisionFromState(now, state, "half_open_transport_failure"), true
}

func decisionFromState(now time.Time, state *rateLimitState, source string) RateLimitDecision {
	delay := state.blockedUntil.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return RateLimitDecision{
		BlockedUntil: state.blockedUntil, Delay: delay, Source: source, Failures: state.failures,
	}
}

func retryDeadline(now time.Time, headers []Header) (time.Time, string, bool) {
	if value := firstHeader(headers, "retry-after"); value != "" {
		if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
			return now.Add(time.Duration(seconds) * time.Second), "retry-after", true
		}
		if deadline, err := http.ParseTime(strings.TrimSpace(value)); err == nil {
			return deadline, "retry-after", true
		}
	}
	for _, name := range []string{"ratelimit-reset", "x-ratelimit-reset"} {
		value := firstHeader(headers, name)
		seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || seconds < 0 {
			continue
		}
		// X-RateLimit-Reset is commonly an epoch; RateLimit-Reset is commonly
		// a delay. Unix seconds are already ten digits, while a practical
		// relative delay is many orders of magnitude smaller.
		if seconds >= 1_000_000_000 {
			return time.Unix(seconds, 0), name, true
		}
		return now.Add(time.Duration(seconds) * time.Second), name, true
	}
	return time.Time{}, "", false
}

func firstHeader(headers []Header, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return strings.TrimSpace(header.Value)
		}
	}
	return ""
}

func normalizedRateLimitKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}
