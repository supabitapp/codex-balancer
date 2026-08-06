package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCallbackRedeemsTheCodeWithThePkceVerifier(t *testing.T) {
	var sent url.Values
	withTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sent = r.PostForm
		json.NewEncoder(w).Encode(tokenResponse{
			IDToken:      jwtFor("acct-1"),
			AccessToken:  "AT",
			RefreshToken: "RT",
		})
	})

	done := make(chan loginResult, 1)
	handler := callbackHandler(context.Background(), http.DefaultClient, "VERIFIER", "STATE", done)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, callbackPath+"?code=CODE&state=STATE", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "acct-1@example.com") {
		t.Errorf("success page should name the account, got %s", rec.Body)
	}
	if sent.Get("grant_type") != "authorization_code" || sent.Get("code") != "CODE" {
		t.Errorf("exchange sent %v", sent)
	}
	if sent.Get("code_verifier") != "VERIFIER" {
		t.Errorf("code_verifier = %q; without it the exchange cannot prove it started the flow", sent.Get("code_verifier"))
	}
	if sent.Get("redirect_uri") != redirectURI {
		t.Errorf("redirect_uri = %q, want %q", sent.Get("redirect_uri"), redirectURI)
	}

	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.account.id() != "acct-1" || result.account.RefreshToken != "RT" {
		t.Fatalf("account = %+v", result.account)
	}
	if result.account.LastRefresh.IsZero() {
		t.Error("LastRefresh not stamped, so a fresh account looks stale")
	}
}

func TestCallbackIgnoresAForeignState(t *testing.T) {
	done := make(chan loginResult, 1)
	handler := callbackHandler(context.Background(), http.DefaultClient, "VERIFIER", "STATE", done)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, callbackPath+"?code=CODE&state=OTHER", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	select {
	case result := <-done:
		t.Fatalf("a forged callback ended the login: %+v", result)
	default:
	}
}

func TestCallbackReportsTheProvidersRefusal(t *testing.T) {
	done := make(chan loginResult, 1)
	handler := callbackHandler(context.Background(), http.DefaultClient, "VERIFIER", "STATE", done)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		callbackPath+"?state=STATE&error=access_denied&error_description=missing_codex_entitlement", nil))

	result := <-done
	if result.err == nil || !strings.Contains(result.err.Error(), "missing_codex_entitlement") {
		t.Fatalf("err = %v, want the provider's reason", result.err)
	}
}

func TestAuthorizeURLCarriesTheChallengeNotTheVerifier(t *testing.T) {
	verifier := randomToken(64)
	link, err := url.Parse(authorizeURL(challengeFor(verifier), "STATE"))
	if err != nil {
		t.Fatal(err)
	}
	query := link.Query()

	if query.Get("code_challenge") != challengeFor(verifier) || query.Get("code_challenge_method") != "S256" {
		t.Errorf("challenge = %q method = %q", query.Get("code_challenge"), query.Get("code_challenge_method"))
	}
	if strings.Contains(link.String(), verifier) {
		t.Error("the verifier leaked into the browser URL")
	}
	if !strings.Contains(query.Get("scope"), "offline_access") {
		t.Errorf("scope = %q; without offline_access there is no refresh token to pool", query.Get("scope"))
	}
	if query.Get("redirect_uri") != redirectURI {
		t.Errorf("redirect_uri = %q, want %q", query.Get("redirect_uri"), redirectURI)
	}
}
