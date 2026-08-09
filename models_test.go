package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestModelCatalogFiltersCompleteAccountCoverage(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
			b.id(): {testModelEntry("gpt-5.6-sol")},
		},
		"0.1.0",
	)

	allowed := catalog.allowedAccounts([]*Account{a, b}, "gpt-5.6-sol", "")
	if fmt.Sprint(sortedAccountIDs(allowed)) != "[account-b]" {
		t.Fatalf("allowed accounts = %v, want account-b", sortedAccountIDs(allowed))
	}
}

func TestModelCatalogFiltersExplicitServiceTier(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-sol")},
			b.id(): {testModelEntry("gpt-5.6-sol", "priority")},
		},
		"0.1.0",
	)

	allowed := catalog.allowedAccounts([]*Account{a, b}, "gpt-5.6-sol", "priority")
	if fmt.Sprint(sortedAccountIDs(allowed)) != "[account-b]" {
		t.Fatalf("allowed accounts = %v, want account-b", sortedAccountIDs(allowed))
	}
	allowed = catalog.allowedAccounts([]*Account{a, b}, "gpt-5.6-sol", "default")
	if fmt.Sprint(sortedAccountIDs(allowed)) != "[account-a account-b]" {
		t.Fatalf("default-tier accounts = %v, want both", sortedAccountIDs(allowed))
	}
}

func TestModelCatalogDoesNotFilterIncompleteCoverage(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
		},
		"0.1.0",
	)

	if allowed := catalog.allowedAccounts([]*Account{a, b}, "gpt-5.6-sol", ""); allowed != nil {
		t.Fatalf("allowed accounts = %v, want unknown", sortedAccountIDs(allowed))
	}
}

func TestModelCatalogIgnoresUnavailableAccountCoverage(t *testing.T) {
	a := testAccount("account-a", 0)
	b := testAccount("account-b", 20)
	c := testAccount("account-c", 40)
	c.dead = "reauth required"
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id(), b.id(), c.id()},
		map[string][]modelEntry{
			a.id(): {testModelEntry("gpt-5.6-terra")},
			b.id(): {testModelEntry("gpt-5.6-sol")},
		},
		"0.1.0",
	)

	allowed := catalog.allowedAccounts([]*Account{a, b, c}, "gpt-5.6-sol", "")
	if fmt.Sprint(sortedAccountIDs(allowed)) != "[account-b]" {
		t.Fatalf("allowed accounts = %v, want account-b", sortedAccountIDs(allowed))
	}
}

func TestModelCatalogInvalidationForcesRefresh(t *testing.T) {
	a := testAccount("account-a", 0)
	catalog := newModelCatalog()
	catalog.replace(
		[]string{a.id()},
		map[string][]modelEntry{a.id(): {testModelEntry("gpt-5.6-sol")}},
		"0.1.0",
	)
	if catalog.needsRefresh([]string{a.id()}, "0.1.0", time.Now()) {
		t.Fatal("fresh catalog needs refresh")
	}
	catalog.invalidate()
	if !catalog.needsRefresh([]string{a.id()}, "0.1.0", time.Now()) {
		t.Fatal("invalidated catalog did not need refresh")
	}
}

func TestModelsRefreshesEveryAccountAndServesUnion(t *testing.T) {
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
			fmt.Fprint(w, `{"models":[{"slug":"gpt-5.6-terra","display_name":"Terra"}]}`)
		case b.id():
			fmt.Fprint(w, `{"models":[{"slug":"gpt-5.6-sol","display_name":"Sol","service_tiers":[{"id":"priority"}]}]}`)
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
	if got := modelSlugs(payload.Models); fmt.Sprint(got) != "[gpt-5.6-sol gpt-5.6-terra]" {
		t.Fatalf("models = %v", got)
	}
	mu.Lock()
	slices.Sort(requests)
	gotRequests := fmt.Sprint(requests)
	mu.Unlock()
	if gotRequests != "[account-a account-b]" {
		t.Fatalf("account requests = %s", gotRequests)
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

func sortedAccountIDs(accounts map[string]bool) []string {
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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
