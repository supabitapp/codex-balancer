package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func jwtFor(id string) string {
	payload, _ := json.Marshal(map[string]any{
		"email": id + "@example.com",
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": id,
			"chatgpt_plan_type":  "pro",
		},
	})
	return "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
}

func testServer(t *testing.T, upstream string, ids ...string) *server {
	t.Helper()
	pool := &Pool{path: filepath.Join(t.TempDir(), "accounts.json")}
	for _, id := range ids {
		pool.accounts = append(pool.accounts, &Account{
			IDToken:     jwtFor(id),
			AccessToken: "token-" + id,
			LastRefresh: time.Now(),
		})
	}
	return &server{
		pool:     pool,
		sticky:   newSticky(),
		upstream: upstream,
		client:   &http.Client{},
		log:      slog.New(slog.DiscardHandler),
	}
}

func sse(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"account\":%q}\n\n", id)
}

func TestClaimsDeriveAccountIdentity(t *testing.T) {
	a := &Account{IDToken: jwtFor("acct-7")}
	if a.ID() != "acct-7" {
		t.Fatalf("ID() = %q, want acct-7", a.ID())
	}
	if a.Email() != "acct-7@example.com" {
		t.Fatalf("Email() = %q", a.Email())
	}
	if a.Plan() != "pro" {
		t.Fatalf("Plan() = %q", a.Plan())
	}
}

func TestRateLimitedAccountFailsOver(t *testing.T) {
	var mu sync.Mutex
	var served []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		served = append(served, id)
		mu.Unlock()
		if id == "acct-a" {
			w.Header().Set("x-codex-primary-used-percent", "100")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		sse(w, id)
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a", "acct-b")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":[]}`))
	s.responses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "acct-b") {
		t.Fatalf("body did not come from the healthy account: %s", rec.Body)
	}
	if len(served) != 2 {
		t.Fatalf("upstream calls = %v, want the 429 then the retry", served)
	}
}

func TestStickySurvivesCompaction(t *testing.T) {
	var mu sync.Mutex
	var served []string
	var bodies []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		served = append(served, r.Header.Get("chatgpt-account-id"))
		bodies = append(bodies, string(body))
		mu.Unlock()
		sse(w, r.Header.Get("chatgpt-account-id"))
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a", "acct-b")

	turn := `{"input":[{"type":"message","role":"user"}]}`
	compaction := `{"input":[{"type":"message","role":"user"},{"type":"compaction_trigger"}]}`

	for _, body := range []string{turn, compaction, turn} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("session-id", "thread-1")
		s.responses(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}

	if served[0] != served[1] || served[1] != served[2] {
		t.Fatalf("thread hopped accounts across compaction: %v", served)
	}
	if bodies[1] != compaction {
		t.Fatalf("compaction body was rewritten:\n got %s\nwant %s", bodies[1], compaction)
	}
}

func TestDifferentThreadsSpreadAcrossAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, r.Header.Get("chatgpt-account-id"))
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a", "acct-b")

	seen := map[string]bool{}
	for _, thread := range []string{"one", "two"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("session-id", thread)
		s.responses(rec, req)
		seen[s.sticky.get(thread)] = true
	}
	if len(seen) != 2 {
		t.Fatalf("both threads landed on the same account: %v", seen)
	}
}

func TestBearerKeyGuardsTheProxy(t *testing.T) {
	s := testServer(t, "http://unused", "acct-a")
	s.key = "secret"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	s.responses(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	s.responses(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key accepted, status = %d", rec.Code)
	}
}

func TestClientHeadersReachUpstreamButAuthDoesNot(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		sse(w, r.Header.Get("chatgpt-account-id"))
	}))
	defer upstream.Close()

	s := testServer(t, upstream.URL, "acct-a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
	s.responses(rec, req)

	if got.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator not forwarded: %v", got)
	}
	if got.Get("x-codex-beta-features") != "remote_compaction_v2" {
		t.Fatalf("beta features header not forwarded: %v", got)
	}
	if got.Get("Authorization") != "Bearer token-acct-a" {
		t.Fatalf("upstream saw %q, want the account token", got.Get("Authorization"))
	}
}

func TestModelsStubKeepsBundledCatalog(t *testing.T) {
	s := testServer(t, "http://unused", "acct-a")
	rec := httptest.NewRecorder()
	s.models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var payload struct {
		Models []any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 0 {
		t.Fatalf("models must stay empty so Codex keeps its compiled-in catalog: %v", payload.Models)
	}
}

func TestWebsocketHandshakeGets426(t *testing.T) {
	s := testServer(t, "http://unused", "acct-a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426; only 426 makes Codex fall back to HTTP", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("426 must carry no body, got %q", rec.Body)
	}
}
