package app

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type transcriptionForm struct {
	filename    string
	contentType string
	audio       []byte
	fields      map[string]string
}

func (f transcriptionForm) encode(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for name, value := range f.fields {
		form.WriteField(name, value)
	}
	if f.audio != nil {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, f.filename))
		header.Set("Content-Type", f.contentType)
		part, err := form.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(f.audio)
	}
	form.Close()
	return &body, form.FormDataContentType()
}

func postTranscription(t *testing.T, proxyURL string, body io.Reader, contentType string, headers http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/audio/transcriptions", body)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		req.Header[name] = values
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(data)
}

func speechForm() transcriptionForm {
	return transcriptionForm{
		filename:    "dictation-2026.wav",
		contentType: "audio/wav",
		audio:       []byte("RIFF-synthetic-audio"),
		fields:      map[string]string{"model": "whisper-1", "response_format": "json", "language": "en"},
	}
}

func TestTranscriptionForwardsDesktopUploadAndReturnsOnlyText(t *testing.T) {
	type upstreamCall struct {
		path, auth, account, originator, userAgent, cookie, codexHeader string
		filename, partType, audio                                       string
		fields                                                          map[string]string
	}
	calls := make(chan upstreamCall, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCall{
			path: r.URL.Path, auth: r.Header.Get("Authorization"), account: r.Header.Get("Chatgpt-Account-Id"),
			originator: r.Header.Get("Originator"), userAgent: r.Header.Get("User-Agent"),
			cookie: r.Header.Get("Cookie"), codexHeader: r.Header.Get("X-Codex-Window-Id"), fields: map[string]string{},
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				call.filename, call.partType, call.audio = part.FileName(), part.Header.Get("Content-Type"), string(data)
				continue
			}
			call.fields[part.FormName()] = string(data)
		}
		calls <- call
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-1")
		io.WriteString(w, `{"text":"hello from the pool","asset_pointer":"sediment://file_123","asset_ttl":"30d","asset_format":"wav"}`)
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL+"/codex", []*Account{testAccount("a", 0)})
	body, contentType := speechForm().encode(t)
	resp, got := postTranscription(t, proxy.URL, body, contentType, http.Header{"Cookie": {"private"}, "X-Codex-Window-Id": {"w1"}})
	if resp.StatusCode != http.StatusOK || got != "{\"text\":\"hello from the pool\"}\n" || resp.Header.Get("X-Request-Id") != "req-1" {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, got)
	}
	call := <-calls
	want := upstreamCall{
		path: "/transcribe", auth: "Bearer token-a", account: "a",
		originator: "Codex Desktop", userAgent: transcriptionUserAgent,
		filename: "codex.wav", partType: "audio/wav", audio: "RIFF-synthetic-audio",
		fields: map[string]string{"language": "en"},
	}
	if fmt.Sprint(call) != fmt.Sprint(want) || !strings.HasPrefix(call.userAgent, "Codex Desktop/") {
		t.Fatalf("upstream call = %+v\nwant %+v", call, want)
	}
}

func TestTranscriptionFailsOverRejectedAccounts(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		account := r.Header.Get("Chatgpt-Account-Id")
		mu.Lock()
		calls = append(calls, account)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch account {
		case "spent":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"quota"}}`)
		case "cooling":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
		default:
			io.WriteString(w, `{"text":"ok"}`)
		}
	}))
	defer upstream.Close()
	spent, cooling, fresh := testAccount("spent", 0), testAccount("cooling", 10), testAccount("fresh", 50)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{spent, cooling, fresh})
	body, contentType := speechForm().encode(t)
	resp, got := postTranscription(t, proxy.URL, body, contentType, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got, `"text":"ok"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	mu.Lock()
	order := fmt.Sprint(calls)
	mu.Unlock()
	if order != "[spent cooling fresh]" {
		t.Fatalf("upstream calls = %s", order)
	}
	if !spent.routingCandidate().spent {
		t.Fatal("usage-limited account was not marked spent")
	}
	if cooling.routingCandidate().cooldown.IsZero() || cooling.routingCandidate().spent {
		t.Fatal("rate-limited account was not cooled")
	}
}

func TestTranscriptionReturnsLastRedactedRejectionWhenPoolExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down token-`+r.Header.Get("Chatgpt-Account-Id")+`"}}`)
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 10)})
	body, contentType := speechForm().encode(t)
	resp, got := postTranscription(t, proxy.URL, body, contentType, nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "9" || !strings.Contains(got, "rate_limit_exceeded") || strings.Contains(got, "token-") {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, got)
	}
}

func TestTranscriptionRefreshRetainsAccount(t *testing.T) {
	refreshes := useOAuthRefreshServer(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		if r.Header.Get("Chatgpt-Account-Id") != "a" {
			t.Error("credential refresh switched accounts")
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"text":"refreshed"}`)
	}))
	defer upstream.Close()
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0), testAccount("b", 0)})
	body, contentType := speechForm().encode(t)
	resp, got := postTranscription(t, proxy.URL, body, contentType, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got, "refreshed") || calls.Load() != 2 || refreshes() != 1 {
		t.Fatalf("status=%d body=%s attempts=%d refreshes=%d", resp.StatusCode, got, calls.Load(), refreshes())
	}
}

func TestTranscriptionChallengeDoesNotPenalizeAccounts(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<html>Just a moment...</html>")
	}))
	defer upstream.Close()
	a, b := testAccount("a", 0), testAccount("b", 0)
	_, proxy := newWebSocketProxy(t, upstream.URL, []*Account{a, b})
	body, contentType := speechForm().encode(t)
	resp, got := postTranscription(t, proxy.URL, body, contentType, nil)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(got, "<html>") || calls.Load() != 1 {
		t.Fatalf("status=%d body=%s calls=%d", resp.StatusCode, got, calls.Load())
	}
	for _, account := range []*Account{a, b} {
		candidate := account.routingCandidate()
		if candidate.spent || !candidate.cooldown.IsZero() {
			t.Fatalf("%s penalized for a client identity challenge", account.id())
		}
	}
}

func TestTranscriptionRejectsInvalidUploadsBeforeUpstream(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{testAccount("a", 0)})
	oversized := speechForm()
	oversized.audio = make([]byte, maxTranscriptionBody)
	missing := speechForm()
	missing.audio = nil
	longLanguage := speechForm()
	longLanguage.fields = map[string]string{"language": strings.Repeat("x", maxTranscriptionLanguage+1)}
	for _, tc := range []struct {
		name string
		form transcriptionForm
		want int
	}{
		{"oversized", oversized, http.StatusRequestEntityTooLarge},
		{"missing file", missing, http.StatusBadRequest},
		{"long language", longLanguage, http.StatusBadRequest},
	} {
		body, contentType := tc.form.encode(t)
		if resp, got := postTranscription(t, proxy.URL, body, contentType, nil); resp.StatusCode != tc.want {
			t.Errorf("%s: status=%d body=%s", tc.name, resp.StatusCode, got)
		}
	}
	if resp, _ := postTranscription(t, proxy.URL, strings.NewReader(`{"file":"x"}`), "application/json", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("json body status = %d", resp.StatusCode)
	}
	srv.lookupAPIKey = func(string) (string, bool, error) { return "", false, nil }
	body, contentType := speechForm().encode(t)
	if resp, _ := postTranscription(t, proxy.URL, body, contentType, http.Header{"Authorization": {"Bearer wrong"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d", resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid uploads reached upstream %d times", calls.Load())
	}
}

func TestTranscriptionFilenameKeepsOnlyExtension(t *testing.T) {
	for name, want := range map[string]string{
		"clip.WAV":            "codex.wav",
		"clip2-aac.mp4":       "codex.mp4",
		"blob":                "codex.webm",
		"notes.tar.gz":        "codex.gz",
		"weird.ext;x=y":       "codex.webm",
		"private name.webm":   "codex.webm",
		"recording.toolongex": "codex.webm",
	} {
		if got := transcriptionFilename(name); got != want {
			t.Errorf("transcriptionFilename(%q) = %q, want %q", name, got, want)
		}
	}
}
