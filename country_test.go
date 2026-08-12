package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestCountryResolverTracksLivePublicIPs(t *testing.T) {
	resolver := countryResolver{}
	threads := []ThreadSnapshot{
		{ClientIP: "8.8.8.8"},
		{ClientIP: "8.8.8.8"},
		{ClientIP: "2001:4860:4860::8888"},
		{ClientIP: "10.0.0.1"},
		{ClientIP: "invalid"},
	}
	ips := resolver.queue(threads)
	slices.Sort(ips)
	want := []string{"2001:4860:4860::8888", "8.8.8.8"}
	if !slices.Equal(ips, want) {
		t.Fatalf("country lookup IPs = %v, want %v", ips, want)
	}
	if repeated := resolver.queue(threads); len(repeated) != 0 {
		t.Fatalf("repeated country lookup IPs = %v", repeated)
	}
	resolver.apply(ips, map[string]string{"8.8.8.8": "US"})
	if got := resolver.label("8.8.8.8"); got != "🇺🇸 US" {
		t.Fatalf("country label = %q, want %q", got, "🇺🇸 US")
	}
	resolver.queue(nil)
	if len(resolver.states) != 0 {
		t.Fatalf("country cache retained inactive IPs: %v", resolver.states)
	}
}

func TestCountryResolverLimitsEachBatch(t *testing.T) {
	threads := make([]ThreadSnapshot, countryLookupBatchMax+1)
	for i := range threads {
		threads[i].ClientIP = fmt.Sprintf("8.8.0.%d", i+1)
	}
	resolver := countryResolver{}
	first := resolver.queue(threads)
	if got := len(first); got != countryLookupBatchMax {
		t.Fatalf("first batch size = %d, want %d", got, countryLookupBatchMax)
	}
	resolver.apply(first, nil)
	if got := len(resolver.queue(threads)); got != 1 {
		t.Fatalf("second batch size = %d, want 1", got)
	}
}

func TestFetchCountryCodesBatchesLookups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q, want application/json", got)
		}
		var ips []string
		if err := json.NewDecoder(r.Body).Decode(&ips); err != nil {
			t.Error(err)
			return
		}
		if want := []string{"8.8.8.8", "1.1.1.1"}; !slices.Equal(ips, want) {
			t.Errorf("request IPs = %v, want %v", ips, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ip":"8.8.8.8","country":"us"},{"ip":"1.1.1.1","country":"AU"}]`))
	}))
	defer server.Close()

	codes, err := fetchCountryCodes(context.Background(), server.Client(), server.URL, []string{"8.8.8.8", "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if codes["8.8.8.8"] != "US" || codes["1.1.1.1"] != "AU" {
		t.Fatalf("country codes = %v", codes)
	}
}
