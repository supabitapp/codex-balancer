package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRoutingCooldownRecoveryUsesEligibleCandidates(t *testing.T) {
	now := time.Now()
	owner, credit := testAccount("owner", 0), testAccountWithCredits("credit")
	paused, unknown := testAccount("paused", 0), testAccount("unknown", 0)
	owner.cooldown, credit.cooldown = now.Add(time.Minute), now.Add(20*time.Second)
	paused.cooldown, unknown.cooldown = now.Add(time.Second), now.Add(2*time.Second)
	paused.Paused = true
	unknown.primary, unknown.secondary = window{}, window{}
	pool := &Pool{accounts: []*Account{paused, unknown, owner, credit}}
	decision := pool.route(nil, map[string]bool{"owner": true, "credit": true})
	if !decision.retryAt.Equal(credit.cooldown) {
		t.Fatalf("fresh recovery = %s, want eligible credit deadline %s", decision.retryAt, credit.cooldown)
	}
	if got := decision.cooldownRecovery(map[string]bool{"owner": true}); !got.Equal(owner.cooldown) {
		t.Fatalf("model-filtered recovery = %s, want owner deadline", got)
	}
	decision = pool.route([]string{"owner"}, nil)
	if !decision.retryAt.Equal(owner.cooldown) {
		t.Fatal("a weaker candidate shortened the retained owner's retry hint")
	}
	if decision := pool.route([]string{"unknown"}, nil); !decision.retryAt.IsZero() {
		t.Fatal("unknown quota cannot promise recovery when its cooldown expires")
	}
}

func TestRoutingRetryHintOverridesPreviousRejection(t *testing.T) {
	decision := routingDecision{retryAt: time.Now().Add(17*time.Second + 500*time.Millisecond)}
	err := &websocketSetupError{
		cause: decision.unavailable(errNoAccountAvailable), status: 429,
		details: responseErrorPayload{Code: "rate_limit_exceeded"}, retryAfter: "300",
	}
	if !errors.Is(err, errNoAccountAvailable) {
		t.Fatal("retry metadata lost error identity")
	}
	_, headers := responseSetupFailure(context.Background(), nil, err)
	if got := headers.Get("Retry-After"); got != "18" {
		t.Fatalf("retry hint = %s, want current deadline rounded up to 18", got)
	}
}

func TestResponsesReportRoutingCooldown(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, transport := range []string{"http", "websocket"} {
			t.Run(strconv.FormatBool(owner)+"/"+transport, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					t.Error("cooling route sent an upstream request")
				}))
				defer upstream.Close()
				a, b := testAccount("a", 0), testAccountWithCredits("b")
				a.cooldown, b.cooldown = time.Now().Add(90*time.Second), time.Now().Add(20*time.Second)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
				headers := codexWebSocketHeaders("session", "thread")
				want := 20
				if owner {
					want = 90
					if !srv.stats.persistRoute(storedRoute{At: time.Now(), Session: "session", Thread: "thread", Account: "a"}) {
						t.Fatal("could not establish owner")
					}
				}
				var response *http.Response
				if transport == "http" {
					response = postResponse(t, proxy.URL, `{"model":"m"}`, headers)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
					if conn != nil {
						conn.CloseNow()
					}
					if err == nil {
						t.Fatal("cooling pool accepted connection")
					}
					response = resp
				}
				if response == nil {
					t.Fatal("missing unavailable response")
				}
				defer response.Body.Close()
				delay, err := strconv.Atoi(response.Header.Get("Retry-After"))
				if response.StatusCode != 503 || err != nil || delay < want-1 || delay > want {
					t.Fatalf("status=%d Retry-After=%q, want503 and about%d seconds", response.StatusCode, response.Header.Get("Retry-After"), want)
				}
			})
		}
	}
}
