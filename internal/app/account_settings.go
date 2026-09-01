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
	endpoint, err := url.Parse(accountSettingsEndpoint)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set("feature", "training_allowed")
	query.Set("value", "false")
	endpoint.RawQuery = query.Encode()

	account.mu.Lock()
	token := account.AccessToken
	accountID := claimsFromToken(account.IDToken).Auth.AccountID
	account.mu.Unlock()
	if token == "" || accountID == "" {
		return errors.New("disable training: account credentials are incomplete")
	}

	requestContext, cancel := context.WithTimeout(ctx, accountSettingsTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPatch, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("chatgpt-account-id", accountID)

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("disable training: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("disable training: account settings returned %s", response.Status)
	}
	return nil
}
