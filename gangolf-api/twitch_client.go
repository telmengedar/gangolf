package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errTwitchCredential = errors.New("credential_unavailable")
var errTwitchAuth = errors.New("auth_required")
var errTwitchForbidden = errors.New("subscription_forbidden")
var errTwitchProtocol = errors.New("invalid_twitch_protocol")
var errTwitchTransport = errors.New("twitch_unavailable")
var errTwitchOverflow = errors.New("inbound_queue_full")

type twitchClient struct {
	rateMu          sync.Mutex
	retryAt         time.Time
	http            *http.Client
	validateURL     string
	tokenURL        string
	subscriptionURL string
	sendURL         string
	socketURL       string
	ircURL          string
	ircTLS          *tls.Config
	directory       string
}

func productionTwitchClient(directory string) *twitchClient {
	return &twitchClient{http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, validateURL: twitchValidateAddress, tokenURL: twitchTokenAddress, subscriptionURL: twitchSubscriptionAddress, sendURL: twitchSendAddress, socketURL: twitchSocketAddress, directory: directory, ircURL: "tls://irc.chat.twitch.tv:6697"}
}

func (c *twitchClient) call(ctx context.Context, method, address, authorization, clientID, body, contentType string) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, strings.NewReader(body))
	if err != nil {
		return nil, 0, nil, errTwitchTransport
	}
	if method == http.MethodPost {
		req.GetBody = nil
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if clientID != "" {
		req.Header.Set("Client-Id", clientID)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, 0, nil, errTwitchTransport
	}
	defer response.Body.Close()
	if response.StatusCode == 429 {
		c.rateMu.Lock()
		c.retryAt = time.Now().Add(retryDelay(response.Header, time.Now()))
		c.rateMu.Unlock()
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, response.StatusCode, response.Header, errTwitchTransport
	}
	return data, response.StatusCode, response.Header, nil
}

func (c *twitchClient) validate(ctx context.Context, p twitchProfile, credential twitchCredential) error {
	return c.validateScopes(ctx, p, credential, "user:read:chat")
}

func (c *twitchClient) validateScopes(ctx context.Context, p twitchProfile, credential twitchCredential, scopes ...string) error {
	return c.validateCredential(ctx, p, &credential, scopes...)
}

func (c *twitchClient) validateCredential(ctx context.Context, p twitchProfile, credential *twitchCredential, scopes ...string) error {
	data, status, _, err := c.call(ctx, "GET", c.validateURL, "OAuth "+credential.AccessToken, "", "", "")
	if err != nil {
		return err
	}
	if status == 401 {
		return errTwitchAuth
	}
	if status != 200 {
		return errTwitchTransport
	}
	var result struct {
		Login     string   `json:"login"`
		ClientID  string   `json:"client_id"`
		UserID    string   `json:"user_id"`
		Scopes    []string `json:"scopes"`
		ExpiresIn int      `json:"expires_in"`
	}
	if json.Unmarshal(data, &result) != nil || result.ClientID != p.ClientID || result.UserID != p.BotUserID || p.Transport == "irc" && result.Login != credential.Login {
		return errTwitchForbidden
	}
	if p.Transport == "irc" {
		interval := time.Hour
		if result.ExpiresIn > 0 {
			interval = min(interval, time.Duration(min(result.ExpiresIn, 7200))*time.Second/2)
		}
		credential.revalidateAt = time.Now().Add(interval)
		credential.diagnostic = ""
		if result.ExpiresIn <= 0 || !slices.Contains(result.Scopes, "chat:read") || !slices.Contains(result.Scopes, "chat:edit") {
			credential.diagnostic = "irc_metadata_unconfirmed"
		}
		return nil
	}
	if result.ExpiresIn <= 0 {
		return errTwitchForbidden
	}
	for _, scope := range scopes {
		if !slices.Contains(result.Scopes, scope) {
			return errTwitchForbidden
		}
	}
	return nil
}

func (c *twitchClient) refreshToken(ctx context.Context, p twitchProfile, credential twitchCredential) (twitchCredential, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}, "client_id": {p.ClientID}}
	if credential.ClientSecret != "" {
		form.Set("client_secret", credential.ClientSecret)
	}
	data, status, _, err := c.call(ctx, "POST", c.tokenURL, "", "", form.Encode(), "application/x-www-form-urlencoded")
	if err != nil {
		return twitchCredential{}, err
	}
	if status == 429 || status >= 500 {
		return twitchCredential{}, errTwitchTransport
	}
	if status != 200 {
		return twitchCredential{}, errTwitchAuth
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(data, &result) != nil || result.AccessToken == "" || result.RefreshToken == "" {
		return twitchCredential{}, errTwitchAuth
	}
	credential.AccessToken, credential.RefreshToken = result.AccessToken, result.RefreshToken
	if err := writeTwitchCredential(c.directory, p.CredentialRef, credential); err != nil {
		return twitchCredential{}, err
	}
	return credential, nil
}

func (c *twitchClient) authorize(ctx context.Context, p twitchProfile) (twitchCredential, error) {
	if p.Transport == "irc" {
		return c.authorizeScopes(ctx, p, "chat:read")
	}
	return c.authorizeScopes(ctx, p, "user:read:chat")
}

func (c *twitchClient) authorizeSend(ctx context.Context, p twitchProfile) (twitchCredential, error) {
	if p.Transport == "irc" {
		return c.authorizeScopes(ctx, p, "chat:read", "chat:edit")
	}
	if _, err := c.sendAddress(); err != nil {
		return twitchCredential{}, err
	}
	return c.authorizeScopes(ctx, p, "user:read:chat", "user:write:chat")
}

func (c *twitchClient) authorizeScopes(ctx context.Context, p twitchProfile, scopes ...string) (twitchCredential, error) {
	unlock, err := lockTwitchGrant(ctx, p)
	if err != nil {
		return twitchCredential{}, err
	}
	defer unlock()
	credential, err := readProfileCredential(c.directory, p)
	if err != nil {
		return credential, err
	}
	err = c.validateCredential(ctx, p, &credential, scopes...)
	if errors.Is(err, errTwitchAuth) && p.Transport != "irc" {
		credential, err = c.refreshToken(ctx, p, credential)
		if err == nil {
			err = c.validateCredential(ctx, p, &credential, scopes...)
		}
	}
	return credential, err
}

func (c *twitchClient) refresh(ctx context.Context, p twitchProfile, previous twitchCredential) (twitchCredential, error) {
	unlock, err := lockTwitchGrant(ctx, p)
	if err != nil {
		return twitchCredential{}, err
	}
	defer unlock()
	current, err := readProfileCredential(c.directory, p)
	if err != nil {
		return twitchCredential{}, err
	}
	if current != previous {
		err := c.validate(ctx, p, current)
		if !errors.Is(err, errTwitchAuth) {
			return current, err
		}
	}
	current, err = c.refreshToken(ctx, p, current)
	if err == nil {
		err = c.validate(ctx, p, current)
	}
	return current, err
}

func retryDelay(headers http.Header, now time.Time) time.Duration {
	delay := time.Second
	if value, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && value > 0 {
		delay = time.Duration(value) * time.Second
	} else if date, err := http.ParseTime(headers.Get("Retry-After")); err == nil && date.After(now) {
		delay = date.Sub(now)
	} else if reset, err := strconv.ParseInt(headers.Get("Ratelimit-Reset"), 10, 64); err == nil && reset > now.Unix() {
		delay = time.Unix(reset, 0).Sub(now)
	}
	return delay
}

func waitTwitch(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *twitchClient) subscribe(ctx context.Context, p twitchProfile, session string, credential *twitchCredential) (string, error) {
	payload, _ := json.Marshal(map[string]any{"type": "channel.chat.message", "version": "1", "condition": map[string]string{"broadcaster_user_id": p.BroadcasterUserID, "user_id": p.BotUserID}, "transport": map[string]string{"method": "websocket", "session_id": session}})
	refreshed := false
	for attempt := 0; attempt < 4; attempt++ {
		data, status, headers, err := c.call(ctx, "POST", c.subscriptionURL, "Bearer "+credential.AccessToken, p.ClientID, string(payload), "application/json")
		if err == nil && status == 202 {
			var result struct {
				Data []twitchSubscription `json:"data"`
			}
			if json.Unmarshal(data, &result) != nil || len(result.Data) != 1 || !result.Data[0].matches(p, session, "") {
				return "", errTwitchProtocol
			}
			return result.Data[0].ID, nil
		}
		if status == 401 && !refreshed {
			refreshed = true
			updated, err := c.refresh(ctx, p, *credential)
			if err != nil {
				return "", err
			}
			*credential = updated
			continue
		}
		if status == 401 {
			return "", errTwitchAuth
		}
		if status == 403 {
			return "", errTwitchForbidden
		}
		if err == nil && status != 429 && status < 500 {
			return "", errTwitchProtocol
		}
		if err := waitTwitch(ctx, retryDelay(headers, time.Now())); err != nil {
			return "", errTwitchTransport
		}
	}
	return "", errTwitchTransport
}

func (c *twitchClient) waitRateLimit(ctx context.Context) error {
	c.rateMu.Lock()
	until := c.retryAt
	c.rateMu.Unlock()
	if delay := time.Until(until); delay > 0 {
		return waitTwitch(ctx, delay)
	}
	return nil
}
