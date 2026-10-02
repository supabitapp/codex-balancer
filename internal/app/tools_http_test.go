package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestToolEndpointsProxyWithPoolCredentials(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		account := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		calls = append(calls, account+" "+r.URL.Path)
		mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token-"+account || r.Header.Get("Cookie") != "" || r.Header.Get("X-Codex-Window-Id") != "w1" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("upstream request = %s %v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		if account == "spent" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
			return
		}
		w.Header().Set("X-Request-Id", "req-1")
		fmt.Fprintf(w, `{"echo":%s,"path":%q}`, body, r.URL.Path)
	}))
	defer upstream.Close()
	spent, fresh := testAccount("spent", 0), testAccount("fresh", 50)
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{spent, fresh})
	post := func(path, key string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(`{"model":"m","input":"find"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", "private")
		req.Header.Set("X-Codex-Window-Id", "w1")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	for _, path := range toolEndpoints {
		resp, body := post(path, "")
		want := fmt.Sprintf(`"path":%q`, strings.TrimPrefix(path, "/v1"))
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, want) || !strings.Contains(body, `"input":"find"`) || resp.Header.Get("X-Request-Id") != "req-1" {
			t.Fatalf("%s: status=%d headers=%v body=%s", path, resp.StatusCode, resp.Header, body)
		}
	}
	if !spent.routingCandidate().spent {
		t.Fatal("usage-limited account was not marked spent")
	}
	mu.Lock()
	got := fmt.Sprint(calls)
	mu.Unlock()
	if got != "[spent /alpha/search fresh /alpha/search fresh /images/generations fresh /images/edits]" {
		t.Fatalf("upstream calls = %s", got)
	}
	srv.lookupAPIKey = func(string) (string, bool, error) { return "", false, nil }
	if resp, _ := post("/v1/alpha/search", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
}

func TestToolEndpointsReturnLastRejectionWhenPoolExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down token-`+r.Header.Get("chatgpt-account-id")+`"}}`)
	}))
	defer upstream.Close()
	a, b := testAccount("a", 0), testAccount("b", 10)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	resp, err := http.Post(proxy.URL+"/v1/alpha/search", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "9" || !strings.Contains(string(body), "rate_limit_exceeded") || strings.Contains(string(body), "token-") {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
	}
	for _, account := range []*Account{a, b} {
		if account.routingCandidate().cooldown.IsZero() {
			t.Fatalf("%s not cooled after a transient 429", account.id())
		}
	}
}

func TestToolRequestsShareEqualQuotaAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("Chatgpt-Account-Id"))
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	var accounts []string
	for range 4 {
		resp, err := http.Post(proxy.URL+"/v1/alpha/search", "application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, string(body))
	}
	if got := fmt.Sprint(accounts); got != "[a b a b]" {
		t.Fatalf("tool placement = %s, want alternating accounts", got)
	}
}

func TestToolDoesNotReplayDeliveredPOST(t *testing.T) {
	calls := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls <- r.Header.Get("Chatgpt-Account-Id")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	resp, err := http.Post(proxy.URL+"/v1/images/generations", "application/json", strings.NewReader(`{"model":"m","prompt":"draw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := len(calls); got != 1 {
		t.Errorf("delivered tool POST %d times; want exactly one", got)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for upstream disconnect", resp.StatusCode)
	}
}

func TestToolRedirectDoesNotResendPOST(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/redirected" {
					t.Error("redirect target received tool POST")
					return
				}
				http.Redirect(w, r, "/redirected", status)
			}))
			defer upstream.Close()
			_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
			resp, err := http.Post(proxy.URL+"/v1/images/generations", "application/json", strings.NewReader(`{"model":"m"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != status || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d, want one redirected response", resp.StatusCode, calls.Load())
			}
		})
	}
}

func TestToolRefreshRetainsAccountAfterDispatch(t *testing.T) {
	refreshes := useOAuthRefreshServer(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Chatgpt-Account-Id") != "a" {
			t.Error("credential refresh switched accounts due to last-used timestamp")
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	resp, err := http.Post(proxy.URL+"/v1/alpha/search", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 2 || refreshes() != 1 {
		t.Fatalf("status=%d attempts=%d refreshes=%d", resp.StatusCode, calls.Load(), refreshes())
	}
}

func TestToolTransportFailureSupersedesEarlierRejection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Chatgpt-Account-Id") == "a" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	resp, err := http.Post(proxy.URL+"/v1/images/generations", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, ambiguous delivery must not report the earlier429", resp.StatusCode)
	}
}
