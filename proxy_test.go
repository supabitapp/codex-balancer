package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResponsesRejectsHTTP(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := httptest.NewRecorder()
	new(server).routes().ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestResetHeaderAcceptsHTTPDate(t *testing.T) {
	want := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	headers := http.Header{"Retry-After": {want.Format(http.TimeFormat)}}
	if got := resetHeader(headers); !got.Equal(want) {
		t.Fatalf("reset = %s, want %s", got, want)
	}
}

func TestUpstreamRetryBackoffTotalsFiveSeconds(t *testing.T) {
	var total time.Duration
	for retry := 1; retry <= maxUpstreamRetries; retry++ {
		total += upstreamRetryBackoff(retry)
	}
	if remaining := upstreamRetryBudget - total; remaining < 0 || remaining > time.Nanosecond {
		t.Fatalf("backoff total = %s, want %s", total, upstreamRetryBudget)
	}
}
