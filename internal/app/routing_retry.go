package app

import (
	"strconv"
	"time"
)

// Retry hints describe the next client request, so per-attempt skips no longer
// apply. Model eligibility and the strongest blocked owner still apply.
func (d routingDecision) cooldownRecovery(allowed map[string]bool) time.Time {
	if d.account != nil {
		return time.Time{}
	}
	var earliest time.Time
	for _, candidate := range d.candidates {
		if d.blocked != "" {
			if candidate.id != d.blocked {
				continue
			}
		} else if !accountAllowed(allowed, candidate.id) {
			continue
		}
		until := candidate.cooldown
		if !until.After(d.now) {
			continue
		}
		candidate.cooldown = time.Time{}
		if !candidate.available(d.now) && !candidate.creditAvailable(d.now) {
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest
}

type routeUnavailableError struct {
	cause   error
	retryAt time.Time
}

func (e *routeUnavailableError) Error() string { return e.cause.Error() }
func (e *routeUnavailableError) Unwrap() error { return e.cause }

func (d routingDecision) unavailable(cause error) error {
	if d.retryAt.IsZero() {
		return cause
	}
	return &routeUnavailableError{cause: cause, retryAt: d.retryAt}
}

func retryAfterSeconds(until time.Time) string {
	remaining := time.Until(until)
	seconds := remaining / time.Second
	if remaining%time.Second > 0 {
		seconds++
	}
	return strconv.FormatInt(int64(max(seconds, 1)), 10)
}
