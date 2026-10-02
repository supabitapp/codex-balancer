package app

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
	"strings"
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

func TestModelCatalogEntriesUseModelAndTierUnion(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	base := testModelEntry("gpt-common", "priority", "default")
	base["display_name"] = "Common from account-a"
	base["additional_speed_tiers"] = []any{"fast"}
	other := testModelEntry("gpt-common", "priority", "ultrafast")
	other["display_name"] = "Common from account-b"
	other["additional_speed_tiers"] = []any{"fast", "ultrafast"}
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
	if got := modelSlugs(entries); fmt.Sprint(got) != "[gpt-a-only gpt-b-only gpt-common]" {
		t.Fatalf("models = %v", got)
	}
	want := cloneModelEntry(base)
	want["service_tiers"] = []any{map[string]any{"id": "priority"}, map[string]any{"id": "ultrafast"}}
	want["additional_speed_tiers"] = []any{"fast", "ultrafast"}
	for _, entry := range entries {
		if modelSlug(entry) == "gpt-common" && !reflect.DeepEqual(entry, want) {
			t.Fatalf("model = %#v, want %#v", entry, want)
		}
	}
	if !reflect.DeepEqual(base["additional_speed_tiers"], []any{"fast"}) {
		t.Fatalf("account-a speed tiers changed: %#v", base["additional_speed_tiers"])
	}
}

func TestModelCatalogEntriesUseKnownCatalogsWithIncompleteCoverage(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{a.id(): {testModelEntry("gpt-common")}},
		"0.1.0",
	)
	if got := modelSlugs(catalog.entries()); fmt.Sprint(got) != "[gpt-common]" {
		t.Fatalf("models = %v", got)
	}
}

func TestModelCatalogFiltersAccountsByModelAndServiceTier(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-terra")},
			b.id(): {
				testModelEntry("gpt-sol", "priority", "ultrafast"),
				{"slug": "gpt-additional", "additional_speed_tiers": []any{"ultrafast"}},
			},
		},
		"0.1.0",
	)

	for _, test := range []struct {
		model string
		tier  string
		want  string
	}{
		{model: "gpt-sol", want: "[account-b]"},
		{model: "gpt-sol", tier: "fast", want: "[account-b]"},
		{model: "gpt-sol", tier: "ultrafast", want: "[account-b]"},
		{model: "gpt-additional", tier: "ultrafast", want: "[account-b]"},
		{model: "gpt-terra", want: "[account-a]"},
	} {
		got := allowedAccountIDs(catalog.allowedAccounts([]*Account{a, b}, test.model, test.tier))
		if fmt.Sprint(got) != test.want {
			t.Fatalf("model %q tier %q accounts = %v, want %s", test.model, test.tier, got, test.want)
		}
	}
}

func TestModelCatalogDoesNotFilterIncompleteCoverage(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{a.id(): {testModelEntry("gpt-terra")}},
		"0.1.0",
	)

	if allowed := catalog.allowedAccounts([]*Account{a, b}, "gpt-sol", ""); allowed != nil {
		t.Fatalf("allowed accounts = %v, want unknown", allowedAccountIDs(allowed))
	}
}

func TestModelCatalogIgnoresMissingCatalogForUnavailableAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	b.Paused = true
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id()},
		map[string][]modelEntry{a.id(): {testModelEntry("gpt-terra")}},
		"0.1.0",
	)

	allowed := catalog.allowedAccounts([]*Account{a, b}, "gpt-terra", "")
	if got := fmt.Sprint(allowedAccountIDs(allowed)); got != "[account-a]" {
		t.Fatalf("allowed accounts = %s", got)
	}
}

func TestModelCatalogDoesNotFilterModelWithoutRoutableAccount(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	b.Paused = true
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-terra")},
			b.id(): {testModelEntry("gpt-sol")},
		},
		"0.150.0",
	)

	for _, test := range []struct{ model, tier string }{
		{model: "gpt-astra"},
		{model: "gpt-terra", tier: "priority"},
		{model: "gpt-sol"},
	} {
		if allowed := catalog.allowedAccounts([]*Account{a, b}, test.model, test.tier); allowed != nil {
			t.Fatalf("model %q tier %q accounts = %v, want unknown", test.model, test.tier, allowedAccountIDs(allowed))
		}
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

func TestModelCatalogCoalescesOlderClientVersionsWithinRefreshInterval(t *testing.T) {
	catalog := newModelCatalog()
	catalog.replace(
		[]string{"account-a"},
		map[string][]modelEntry{"account-a": {testModelEntry("gpt-common")}},
		"0.2.0",
	)
	nextRefresh := catalog.nextRefresh
	if catalog.needsRefresh([]string{"account-a"}, catalog.newestVersion("0.1.0"), nextRefresh.Add(-time.Second)) {
		t.Fatal("older client version bypassed refresh interval")
	}
	if !catalog.needsRefresh([]string{"account-a"}, catalog.newestVersion("0.1.0"), nextRefresh) {
		t.Fatal("catalog did not refresh after interval")
	}
	if got := catalog.newestVersion("0.1.0"); got != "0.2.0" {
		t.Fatalf("refresh version = %s, want the newest client version", got)
	}
}

func TestNewerClientVersionComparesNumericParts(t *testing.T) {
	for _, test := range []struct {
		current   string
		candidate string
		want      bool
	}{
		{current: "", candidate: "0.150.0", want: true},
		{current: "0.150.0", candidate: "0.153.4", want: true},
		{current: "0.9.0", candidate: "0.10.0", want: true},
		{current: "0.153.4", candidate: "0.153.4"},
		{current: "0.153.4", candidate: "0.150.0"},
		{current: "0.153.4", candidate: "0.153"},
		{current: "0.153", candidate: "0.153.4", want: true},
		{current: "0.153.4", candidate: "0.153.5-alpha", want: true},
	} {
		if got := newerClientVersion(test.current, test.candidate); got != test.want {
			t.Fatalf("newerClientVersion(%q, %q) = %t", test.current, test.candidate, got)
		}
	}
}

func TestModelsRefreshesEveryActiveAccountAndServesUnion(t *testing.T) {
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
			fmt.Fprint(w, `{"models":[{"slug":"gpt-common","display_name":"A","service_tiers":[{"id":"priority"}],"additional_speed_tiers":["fast"]},{"slug":"gpt-a-only"}]}`)
		case b.id():
			fmt.Fprint(w, `{"models":[{"slug":"gpt-common","display_name":"B","service_tiers":[{"id":"priority"},{"id":"ultrafast","name":"Ultrafast","description":"The fastest available responses for latency-sensitive work."}],"additional_speed_tiers":["fast","ultrafast"]},{"slug":"gpt-b-only"}]}`)
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
	if got := modelSlugs(payload.Models); fmt.Sprint(got) != "[gpt-a-only gpt-b-only gpt-common]" {
		t.Fatalf("models = %v", got)
	}
	for _, model := range payload.Models {
		if modelSlug(model) != "gpt-common" {
			continue
		}
		if model["display_name"] != "A" {
			t.Fatalf("base payload = %#v, want account-a payload", model)
		}
		if !reflect.DeepEqual(model["additional_speed_tiers"], []any{"fast", "ultrafast"}) {
			t.Fatalf("speed tiers = %#v", model["additional_speed_tiers"])
		}
		wantTiers := []any{map[string]any{"id": "priority"}, map[string]any{"id": "ultrafast", "name": "Ultrafast", "description": "The fastest available responses for latency-sensitive work."}}
		if !reflect.DeepEqual(model["service_tiers"], wantTiers) {
			t.Fatalf("service tiers = %#v, want %#v", model["service_tiers"], wantTiers)
		}
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
	b.Reauth = "refresh_token_invalidated"
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

func TestModelsServeNewestClientCatalogWhateverTheClientOrder(t *testing.T) {
	for _, test := range []struct {
		name     string
		clients  []string
		fetched  string
		accounts string
	}{
		{
			name:     "older client keeps the newer catalog and fetches nothing",
			clients:  []string{"0.153.4", "0.150.0"},
			fetched:  "[0.153.4]",
			accounts: "[account-a]",
		},
		{
			name:     "newer client refreshes without waiting for the interval",
			clients:  []string{"0.150.0", "0.153.4"},
			fetched:  "[0.150.0 0.153.4]",
			accounts: "[account-a]",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := testAccount("account-a", 0)
			var mu sync.Mutex
			fetched := []string{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				version := r.URL.Query().Get("client_version")
				mu.Lock()
				fetched = append(fetched, version)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if version == "0.153.4" {
					fmt.Fprint(w, `{"models":[{"slug":"gpt-terra"},{"slug":"gpt-astra"}]}`)
					return
				}
				fmt.Fprint(w, `{"models":[{"slug":"gpt-terra"}]}`)
			}))
			defer upstream.Close()

			server := &server{
				pool:     &Pool{accounts: []*Account{a}},
				catalog:  newModelCatalog(),
				upstream: upstream.URL,
				client:   upstream.Client(),
				log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			for _, clientVersion := range test.clients {
				request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version="+clientVersion, nil)
				response := httptest.NewRecorder()
				server.models(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
				}
			}
			if got := modelSlugs(server.catalog.entries()); fmt.Sprint(got) != "[gpt-astra gpt-terra]" {
				t.Fatalf("models = %v, want the newest client catalog", got)
			}
			mu.Lock()
			gotFetched := fmt.Sprint(fetched)
			mu.Unlock()
			if gotFetched != test.fetched {
				t.Fatalf("upstream client versions = %s, want %s", gotFetched, test.fetched)
			}
			allowed := server.catalog.allowedAccounts([]*Account{a}, "gpt-astra", "")
			if got := fmt.Sprint(allowedAccountIDs(allowed)); got != test.accounts {
				t.Fatalf("allowed accounts = %s, want %s", got, test.accounts)
			}
		})
	}
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

func allowedAccountIDs(allowed map[string]bool) []string {
	ids := make([]string, 0, len(allowed))
	for id := range allowed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestModelsFilterByRequestingClientVersion(t *testing.T) {
	a := testAccount("account-a", 0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"slug":"gpt-old","minimal_client_version":[0,1,0]},{"slug":"gpt-new","minimal_client_version":[0,2,0]},{"slug":"gpt-any"}]}`)
	}))
	defer upstream.Close()
	server := &server{
		pool:     &Pool{accounts: []*Account{a}},
		catalog:  newModelCatalog(),
		upstream: upstream.URL,
		client:   upstream.Client(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for version, want := range map[string]string{"0.2.0": "[gpt-any gpt-new gpt-old]", "0.1.5": "[gpt-any gpt-old]", "0.0.9": "[gpt-any]"} {
		request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version="+version, nil)
		response := httptest.NewRecorder()
		server.models(response, request)
		var payload struct {
			Models []modelEntry `json:"models"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(modelSlugs(payload.Models)); got != want {
			t.Fatalf("client %s models = %s, want %s", version, got, want)
		}
	}
}

func TestModelsServeCachedCatalogWhileRefreshStalls(t *testing.T) {
	a := testAccount("account-a", 0)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"slug":"gpt-late"}]}`)
	}))
	defer upstream.Close()
	server := &server{
		pool:        &Pool{accounts: []*Account{a}},
		catalog:     newModelCatalog(),
		upstream:    upstream.URL,
		client:      upstream.Client(),
		catalogWait: 50 * time.Millisecond,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	started := time.Now()
	response := httptest.NewRecorder()
	server.models(response, httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.1.0", nil))
	if elapsed := time.Since(started); response.Code != http.StatusOK || elapsed > time.Second || response.Body.String() != "{\"models\":[]}\n" {
		t.Fatalf("stalled refresh: status=%d elapsed=%s body=%s", response.Code, elapsed, response.Body.String())
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for len(server.catalog.entries()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = httptest.NewRecorder()
	server.models(response, httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.1.0", nil))
	if !strings.Contains(response.Body.String(), "gpt-late") {
		t.Fatalf("catalog not served after refresh: %s", response.Body.String())
	}
}

func TestModelsPersistAndSeedNewestClientVersion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"slug":"gpt-persisted"}]}`)
	}))
	defer upstream.Close()
	srv := newTestServer(t, []*Account{testAccount("account-a", 0)})
	srv.upstream = upstream.URL
	srv.client = upstream.Client()
	for _, version := range []string{"0.3.0", "0.2.0"} {
		srv.models(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models?client_version="+version, nil))
	}
	if stored, err := srv.pool.store.raw.ModelsClientVersion(); err != nil || stored != "0.3.0" {
		t.Fatalf("stored version = %q, error = %v", stored, err)
	}
	catalog := newModelCatalog()
	catalog.seed("0.3.0")
	if catalog.version() != "0.3.0" || !catalog.needsRefresh([]string{"account-a"}, "0.3.0", time.Now()) {
		t.Fatal("seeded catalog does not schedule the startup refresh")
	}
	catalog.seed("0.1.0")
	if catalog.version() != "0.3.0" {
		t.Fatal("older seed replaced the newest version")
	}
}
