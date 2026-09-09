package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const (
	accountSettingsEndpoint = "https://chatgpt.com/backend-api/settings/account_user_setting"
	accountSettingsTimeout  = 30 * time.Second
)

func connectAccount(ctx context.Context, client *http.Client, tokens tokenResponse) (*Account, error) {
	account := accountFromState(accountState{
		IDToken:      tokens.IDToken,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		LastRefresh:  time.Now(),
	})
	if err := disableTraining(ctx, client, account); err != nil {
		return nil, err
	}
	return account, nil
}

func disableTraining(ctx context.Context, client *http.Client, account *Account) error {
	account.mu.Lock()
	token := account.AccessToken
	claims := claimsFromToken(account.IDToken)
	account.mu.Unlock()
	if token == "" || claims.Auth.AccountID == "" {
		return errors.New("disable training: account credentials are incomplete")
	}
	// Business and Enterprise data is excluded from training by default.
	// These workspaces do not need the personal-account training toggle.
	if managedWorkspacePlan(claims.Auth.Plan) {
		return nil
	}

	endpoint, err := url.Parse(accountSettingsEndpoint)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("feature", "training_allowed")
	query.Set("value", "false")
	endpoint.RawQuery = query.Encode()

	requestContext, cancel := context.WithTimeout(ctx, accountSettingsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPatch, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("chatgpt-account-id", claims.Auth.AccountID)

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("disable training: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("disable training: account settings returned %s (ID token plan: %q)", response.Status, claims.Auth.Plan)
	}
	return nil
}
