package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const deviceAuthTimeout = 15 * time.Minute

const deviceAuthPrompt = `Open this link and sign in:

%s/codex/device

Enter this one-time code (expires in %d minutes):

%s

Waiting for sign-in...
`

type deviceAuthorization struct {
	authID   string
	userCode string
	interval time.Duration
}

type deviceAuthorizationResponse struct {
	ID       string `json:"device_auth_id"`
	Code     string `json:"user_code"`
	Interval string `json:"interval"`
}

type deviceTokenRequest struct {
	ID   string `json:"device_auth_id"`
	Code string `json:"user_code"`
}

type deviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

func loginWithDeviceCode(ctx context.Context, hc *http.Client, out io.Writer, issuer string) (*Account, error) {
	issuer = deviceAuthIssuer(issuer)
	device, err := requestDeviceAuthorization(ctx, hc, issuer+"/api/accounts/deviceauth/usercode")
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(out, deviceAuthPrompt, issuer, deviceAuthTimeout/time.Minute, device.userCode)
	return completeDeviceAuthorization(ctx, hc, issuer, device)
}

func completeDeviceAuthorization(ctx context.Context, hc *http.Client, issuer string, device deviceAuthorization) (*Account, error) {
	issuer = deviceAuthIssuer(issuer)
	code, err := pollDeviceAuthorization(ctx, hc, issuer+"/api/accounts/deviceauth/token", device)
	if err != nil {
		return nil, err
	}
	tokens, err := exchangeCodeAt(
		ctx,
		hc,
		issuer+"/oauth/token",
		code.AuthorizationCode,
		issuer+"/deviceauth/callback",
		code.CodeVerifier,
	)
	if err != nil {
		return nil, fmt.Errorf("device code exchange failed: %w", err)
	}
	return accountFromTokens(tokens), nil
}

func deviceAuthIssuer(issuer string) string {
	return strings.TrimRight(issuer, "/")
}

func deviceVerificationURL(issuer string) string {
	return deviceAuthIssuer(issuer) + "/codex/device"
}

func requestDeviceAuthorization(ctx context.Context, hc *http.Client, endpoint string) (deviceAuthorization, error) {
	var device deviceAuthorization
	resp, err := postJSON(ctx, hc, endpoint, struct {
		ClientID string `json:"client_id"`
	}{ClientID: oauthClientID})
	if err != nil {
		return device, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return device, errors.New("device code login is not enabled; use browser sign-in")
	}
	if resp.StatusCode/100 != 2 {
		return device, fmt.Errorf("device code request failed with status %s", resp.Status)
	}

	var result deviceAuthorizationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return device, err
	}
	interval, err := strconv.ParseUint(strings.TrimSpace(result.Interval), 10, 32)
	if err != nil {
		return device, fmt.Errorf("invalid device code polling interval %q", result.Interval)
	}
	if result.ID == "" || result.Code == "" {
		return device, errors.New("device code response omitted the id or user code")
	}
	return deviceAuthorization{
		authID:   result.ID,
		userCode: result.Code,
		interval: time.Duration(interval) * time.Second,
	}, nil
}

func pollDeviceAuthorization(ctx context.Context, hc *http.Client, endpoint string, device deviceAuthorization) (deviceTokenResponse, error) {
	var code deviceTokenResponse
	pollCtx, cancel := context.WithTimeout(ctx, deviceAuthTimeout)
	defer cancel()

	for {
		resp, err := postJSON(pollCtx, hc, endpoint, deviceTokenRequest{ID: device.authID, Code: device.userCode})
		if err != nil {
			return code, devicePollError(ctx, pollCtx, err)
		}

		if resp.StatusCode/100 == 2 {
			err := json.NewDecoder(resp.Body).Decode(&code)
			resp.Body.Close()
			if err != nil {
				return code, err
			}
			if code.AuthorizationCode == "" || code.CodeVerifier == "" {
				return code, errors.New("device auth response omitted the authorization code or verifier")
			}
			return code, nil
		}

		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return code, fmt.Errorf("device auth failed with status %s", resp.Status)
		}
		if err := waitForDevicePoll(pollCtx, device.interval); err != nil {
			return code, devicePollError(ctx, pollCtx, err)
		}
	}
}

func devicePollError(ctx, pollCtx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if pollCtx.Err() != nil {
		return fmt.Errorf("device auth timed out after %d minutes", deviceAuthTimeout/time.Minute)
	}
	return err
}

func waitForDevicePoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func postJSON(ctx context.Context, hc *http.Client, endpoint string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return hc.Do(req)
}
