package app

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	authBaseURL  = "https://auth.openai.com"
	callbackPort = 1455
	callbackPath = "/auth/callback"
	loginScope   = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

var (
	callbackAddr     = fmt.Sprintf("127.0.0.1:%d", callbackPort)
	redirectURI      = fmt.Sprintf("http://localhost:%d%s", callbackPort, callbackPath)
	errStateMismatch = errors.New("state mismatch")
	errLoginComplete = errors.New("sign-in already completed")
)

type loginResult struct {
	account *Account
	err     error
}

type loginFlow struct {
	ctx      context.Context
	hc       *http.Client
	verifier string
	state    string
	done     chan loginResult
	once     sync.Once
}

func newLoginFlow(ctx context.Context, hc *http.Client, verifier, state string) *loginFlow {
	return &loginFlow{
		ctx:      ctx,
		hc:       hc,
		verifier: verifier,
		state:    state,
		done:     make(chan loginResult, 1),
	}
}

func (f *loginFlow) complete(query url.Values) loginResult {
	if query.Get("state") != f.state {
		return loginResult{err: errStateMismatch}
	}

	result := loginResult{err: errLoginComplete}
	f.once.Do(func() {
		result.account, result.err = redeem(f.ctx, f.hc, query, f.verifier)
		f.done <- result
	})
	return result
}

func login(ctx context.Context, hc *http.Client) (*Account, error) {
	listener, err := net.Listen("tcp", callbackAddr)
	if err != nil {
		return nil, fmt.Errorf("%s is taken; the sign-in page only redirects there, so close what holds it and retry", callbackAddr)
	}
	defer listener.Close()

	verifier := randomToken(64)
	state := randomToken(32)
	flow := newLoginFlow(ctx, hc, verifier, state)

	server := &http.Server{Handler: callbackHandler(flow)}
	go server.Serve(listener)
	defer server.Shutdown(context.Background())

	link := authorizeURL(challengeFor(verifier), state)
	fmt.Fprintf(os.Stderr, "Opening your browser to sign in.\nIf nothing opens, visit:\n\n%s\n\nIf the callback does not open, paste its full URL here:\n", link)
	openBrowser(link)
	go acceptPastedCallbacks(os.Stdin, os.Stderr, flow)

	select {
	case result := <-flow.done:
		return result.account, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func callbackHandler(flow *loginFlow) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		result := flow.complete(r.URL.Query())
		if result.err != nil {
			http.Error(w, result.err.Error(), http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, successPage, html.EscapeString(describe(result.account)))
		}
	})
	return mux
}

func acceptPastedCallbacks(in io.Reader, out io.Writer, flow *loginFlow) {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		query, err := parseCallbackURL(scanner.Text())
		if err != nil {
			fmt.Fprintf(out, "Could not use callback URL: %v\nPaste its full URL here:\n", err)
			continue
		}
		result := flow.complete(query)
		if errors.Is(result.err, errStateMismatch) {
			fmt.Fprintf(out, "Could not use callback URL: %v\nPaste its full URL here:\n", result.err)
			continue
		}
		return
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(out, "Could not read callback URL: %v\n", err)
	}
}

func parseCallbackURL(raw string) (url.Values, error) {
	callback, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	expected, err := url.Parse(redirectURI)
	if err != nil {
		return nil, err
	}
	if callback.Scheme != expected.Scheme ||
		!strings.EqualFold(callback.Hostname(), expected.Hostname()) ||
		callback.Port() != expected.Port() ||
		callback.Path != expected.Path {
		return nil, fmt.Errorf("expected %s", redirectURI)
	}
	return callback.Query(), nil
}

func redeem(ctx context.Context, hc *http.Client, query url.Values, verifier string) (*Account, error) {
	if reason := query.Get("error"); reason != "" {
		return nil, fmt.Errorf("sign-in refused: %s", cmp.Or(query.Get("error_description"), reason))
	}
	code := query.Get("code")
	if code == "" {
		return nil, errors.New("callback carried no authorization code")
	}
	tokens, err := exchangeCode(ctx, hc, code, verifier)
	if err != nil {
		return nil, err
	}
	return accountFromTokens(tokens), nil
}

func accountFromTokens(tokens tokenResponse) *Account {
	return accountFromState(accountState{
		IDToken:      tokens.IDToken,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		LastRefresh:  time.Now(),
	})
}

func exchangeCode(ctx context.Context, hc *http.Client, code, verifier string) (tokenResponse, error) {
	return exchangeCodeAt(ctx, hc, oauthEndpoint, code, redirectURI, verifier)
}

func exchangeCodeAt(ctx context.Context, hc *http.Client, endpoint, code, redirect, verifier string) (tokenResponse, error) {
	var out tokenResponse
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {oauthClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return out, fmt.Errorf("token endpoint returned %s: %s", resp.Status, body)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func authorizeURL(challenge, state string) string {
	query := url.Values{
		"response_type":              {"code"},
		"client_id":                  {oauthClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {loginScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {"codex_cli_rs"},
	}
	return authBaseURL + "/oauth/authorize?" + query.Encode()
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func openBrowser(link string) {
	argv := []string{"xdg-open"}
	switch runtime.GOOS {
	case "darwin":
		argv = []string{"open"}
	case "windows":
		argv = []string{"rundll32", "url.dll,FileProtocolHandler"}
	}
	exec.Command(argv[0], append(argv[1:], link)...).Start()
}

const successPage = `<!doctype html>
<meta charset="utf-8">
<title>Signed in</title>
<body style="font: 16px/1.6 system-ui, sans-serif; display: grid; place-content: center; height: 100vh; margin: 0">
<p>Signed in as %s. Close this tab and go back to the terminal.</p>
`
