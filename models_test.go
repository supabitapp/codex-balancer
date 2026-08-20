package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

type modelTestLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *modelTestLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *modelTestLogBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	decoder := json.NewDecoder(bytes.NewReader(b.Bytes()))
	var records []map[string]any
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func requireModelLogRecord(t *testing.T, records []map[string]any, message string, fields map[string]any) {
	t.Helper()
	for _, record := range records {
		if record["msg"] != message {
			continue
		}
		matched := true
		for name, value := range fields {
			if !reflect.DeepEqual(record[name], value) {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("missing log %q with fields %v in %v", message, fields, records)
}

func TestModelCatalogEntriesUseCommonModelsAndTiers(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	base := testModelEntry("gpt-common", "priority", "default")
	base["display_name"] = "Common from account-a"
	other := testModelEntry("gpt-common", "priority")
	other["display_name"] = "Common from account-b"
	catalog := newModelCatalog()
	catalog.replace(
		[]string{b.id(), a.id()},
		map[string][]modelEntry{
			a.id(): {base, testModelEntry("gpt-a-only")},
			b.id(): {other, testModelEntry("gpt-b-only")},
		},
		"0.1.0",
	)

	entries := catalog.entries()
	if got := modelSlugs(entries); fmt.Sprint(got) != "[gpt-common]" {
		t.Fatalf("models = %v, want common model", got)
	}
	want := cloneModelEntry(base)
	want["service_tiers"] = []any{map[string]any{"id": "priority"}}
	if !reflect.DeepEqual(entries[0], want) {
		t.Fatalf("model = %#v, want %#v", entries[0], want)
	}
}

func TestModelCatalogEntriesFailClosedForIncompleteCoverage(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{a.id(): {testModelEntry("gpt-common")}},
		"0.1.0",
	)
	if got := catalog.entries(); len(got) != 0 {
		t.Fatalf("models = %v, want no advertised models", got)
	}
}

func TestModelCatalogEntriesSortModelsAndChooseStableBase(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	baseA := testModelEntry("gpt-zeta")
	baseA["display_name"] = "from a"
	baseB := testModelEntry("gpt-zeta")
	baseB["display_name"] = "from b"
	catalog := newModelCatalog()
	catalog.replace(
		[]string{b.id(), a.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-zeta"), baseA, testModelEntry("gpt-alpha")},
			b.id(): {baseB, testModelEntry("gpt-alpha")},
		},
		"0.1.0",
	)

	entries := catalog.entries()
	if got := modelSlugs(entries); fmt.Sprint(got) != "[gpt-alpha gpt-zeta]" {
		t.Fatalf("models = %v", got)
	}
	for _, entry := range entries {
		if modelSlug(entry) == "gpt-zeta" && entry["display_name"] != "from a" {
			t.Fatalf("base payload = %#v, want account-a payload", entry)
		}
	}
}

func TestModelCatalogRetainsCachedCatalogAfterRefreshFailure(t *testing.T) {
	catalog := newModelCatalog()
	catalog.replace(
		[]string{"account-a", "account-b"},
		map[string][]modelEntry{
			"account-a": {testModelEntry("gpt-common")},
			"account-b": {testModelEntry("gpt-common")},
		},
		"0.1.0",
	)
	first := catalog.entries()
	catalog.replace(
		[]string{"account-a", "account-b"},
		map[string][]modelEntry{"account-a": {testModelEntry("gpt-common")}},
		"0.1.0",
	)
	if got := catalog.entries(); !reflect.DeepEqual(got, first) {
		t.Fatalf("models after failed refresh = %#v, want %#v", got, first)
	}
}

func TestModelCatalogInvalidationForcesRefresh(t *testing.T) {
	catalog := newModelCatalog()
	catalog.replace(
		[]string{"account-a"},
		map[string][]modelEntry{"account-a": {testModelEntry("gpt-common")}},
		"0.1.0",
	)
	if catalog.needsRefresh([]string{"account-a"}, "0.1.0", time.Now()) {
		t.Fatal("fresh catalog needs refresh")
	}
	catalog.invalidate()
	if !catalog.needsRefresh([]string{"account-a"}, "0.1.0", time.Now()) {
		t.Fatal("invalidated catalog did not need refresh")
	}
}

func TestModelCatalogCoalescesClientVersionsWithinRefreshInterval(t *testing.T) {
	catalog := newModelCatalog()
	catalog.replace(
		[]string{"account-a"},
		map[string][]modelEntry{"account-a": {testModelEntry("gpt-common")}},
		"0.1.0",
	)
	refreshedAt := catalog.refreshedAt
	if catalog.needsRefresh([]string{"account-a"}, "0.2.0", refreshedAt.Add(modelRefreshInterval-time.Second)) {
		t.Fatal("new client version bypassed refresh interval")
	}
	if !catalog.needsRefresh([]string{"account-a"}, "0.2.0", refreshedAt.Add(modelRefreshInterval)) {
		t.Fatal("catalog did not refresh after interval")
	}
}

func TestModelCatalogDerivesContextLimits(t *testing.T) {
	catalog := newModelCatalog()
	entry := testModelEntry("gpt-common")
	entry["context_window"] = json.Number("272000")
	entry["effective_context_window_percent"] = json.Number("95")
	catalog.replace([]string{"account"}, map[string][]modelEntry{"account": {entry}}, "0.147.0")

	limits := catalog.contextLimits("account", "gpt-common-2026-08-09")
	if limits.Window != 258_400 || limits.AutoCompact != 244_800 {
		t.Fatalf("context limits = %+v", limits)
	}
}

func TestModelsRefreshesEveryActiveAccountAndServesIntersection(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	var mu sync.Mutex
	requests := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.URL.Query().Get("client_version") != "0.1.0" {
			t.Errorf("request URL = %s", r.URL.String())
		}
		account := r.Header.Get("chatgpt-account-id")
		mu.Lock()
		requests = append(requests, account)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch account {
		case a.id():
			fmt.Fprint(w, `{"models":[{"slug":"gpt-common","display_name":"A","service_tiers":[{"id":"priority"}]},{"slug":"gpt-a-only"}]}`)
		case b.id():
			fmt.Fprint(w, `{"models":[{"slug":"gpt-common","display_name":"B","service_tiers":[{"id":"priority"}]},{"slug":"gpt-b-only"}]}`)
		default:
			http.Error(w, "unknown account", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	server := &server{
		pool:     &Pool{accounts: []*Account{a, b}},
		catalog:  newModelCatalog(),
		upstream: upstream.URL,
		client:   upstream.Client(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.1.0", nil)
	response := httptest.NewRecorder()
	server.models(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := modelSlugs(payload.Models); fmt.Sprint(got) != "[gpt-common]" {
		t.Fatalf("models = %v", got)
	}
	if payload.Models[0]["display_name"] != "A" {
		t.Fatalf("base payload = %#v, want account-a payload", payload.Models[0])
	}
	mu.Lock()
	slices.Sort(requests)
	gotRequests := fmt.Sprint(requests)
	mu.Unlock()
	if gotRequests != "[account-a account-b]" {
		t.Fatalf("account requests = %s", gotRequests)
	}
}

func TestModelsRefreshSkipsReauthAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	b.dead = "refresh_token_invalidated"
	var mu sync.Mutex
	requests := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Header.Get("chatgpt-account-id"))
		mu.Unlock()
		fmt.Fprint(w, `{"models":[{"slug":"gpt-common"}]}`)
	}))
	defer upstream.Close()
	logs := &modelTestLogBuffer{}
	server := &server{
		pool:     &Pool{accounts: []*Account{a, b}},
		catalog:  newModelCatalog(),
		upstream: upstream.URL,
		client:   upstream.Client(),
		log:      slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.1.0", nil)
	response := httptest.NewRecorder()
	server.models(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := modelSlugs(payload.Models); fmt.Sprint(got) != "[gpt-common]" {
		t.Fatalf("models = %v", got)
	}
	mu.Lock()
	gotRequests := fmt.Sprint(requests)
	mu.Unlock()
	if gotRequests != "[account-a]" {
		t.Fatalf("account requests = %s", gotRequests)
	}
	requireModelLogRecord(t, logs.records(t), "model refresh skipped account", map[string]any{
		"account":        "account-b",
		"client_version": "0.1.0",
		"reason":         "needs_reauth",
	})
}

func TestModelsRefreshFailureWaitsForRefreshInterval(t *testing.T) {
	a := testAccount("account-a", 0)
	var mu sync.Mutex
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	logs := &modelTestLogBuffer{}
	server := &server{
		pool:     &Pool{accounts: []*Account{a}},
		catalog:  newModelCatalog(),
		upstream: upstream.URL,
		client:   upstream.Client(),
		log:      slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.1.0", nil)
		response := httptest.NewRecorder()
		server.models(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 1 {
		t.Fatalf("requests = %d, want one fetch", gotRequests)
	}
	if got := server.catalog.entries(); len(got) != 0 {
		t.Fatalf("models = %v, want no advertised models", got)
	}
	requireModelLogRecord(t, logs.records(t), "model catalog retained after refresh failure", map[string]any{
		"account":        "account-a",
		"client_version": "0.1.0",
		"models":         float64(0),
	})
}

func testModelEntry(slug string, serviceTiers ...string) modelEntry {
	entry := modelEntry{"slug": slug}
	if len(serviceTiers) == 0 {
		return entry
	}
	tiers := make([]any, 0, len(serviceTiers))
	for _, serviceTier := range serviceTiers {
		tiers = append(tiers, map[string]any{"id": serviceTier})
	}
	entry["service_tiers"] = tiers
	return entry
}

func modelSlugs(models []modelEntry) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slug, _ := model["slug"].(string)
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)
	return slugs
}
