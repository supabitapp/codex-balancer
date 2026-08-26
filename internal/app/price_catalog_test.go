package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestPriceCatalogRefreshPersistsLastValidSnapshot(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 11, 15, 0, 0, 0, time.UTC)
	valid := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if valid {
			w.Write([]byte(testModelsDevCatalog))
			return
		}
		w.Write([]byte(`{"openai":{"models":{}}}`))
	}))
	defer upstream.Close()

	catalog, err := newPriceCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	catalog.endpoint = upstream.URL
	catalog.client = upstream.Client()
	catalog.now = func() time.Time { return now }
	snapshot, err := catalog.refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	catalog.install(snapshot)
	valid = false
	if _, err := catalog.refresh(context.Background()); err == nil {
		t.Fatal("invalid refresh succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := newPriceCatalog(reopened)
	if err != nil {
		t.Fatal(err)
	}
	got, known := restored.current().estimate("gpt-5.4", "default", responseUsage{InputTokens: 1_000})
	if !known || got != 2_500_000 {
		t.Fatalf("restored estimate = %d, %t, want 2500000, true", got, known)
	}
	if !restored.current().fetchedAt.Equal(now) {
		t.Fatalf("fetched at = %s, want %s", restored.current().fetchedAt, now)
	}
}

func TestPriceCatalogRefreshScheduleUsesSnapshotAge(t *testing.T) {
	now := time.Date(2026, time.August, 11, 15, 0, 0, 0, time.UTC)
	catalog, err := newPriceCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.refreshIn(now); got != 0 {
		t.Fatalf("empty refresh delay = %s, want zero", got)
	}
	catalog.install(priceSnapshot{fetchedAt: now})
	if got := catalog.refreshIn(now.Add(23 * time.Hour)); got != time.Hour {
		t.Fatalf("fresh refresh delay = %s, want 1h", got)
	}
	if got := catalog.refreshIn(now.Add(24 * time.Hour)); got != 0 {
		t.Fatalf("stale refresh delay = %s, want zero", got)
	}
}
