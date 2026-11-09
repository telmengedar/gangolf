package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (c *twitchClient) sendMessage(ctx context.Context, p twitchProfile, credential twitchCredential, text string) (string, string, string, int) {
	unlock, err := lockTwitchGrant(ctx, p)
	if err != nil {
		return "unknown", "", "", 0
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"broadcaster_id": p.BroadcasterUserID, "sender_id": p.BotUserID, "message": text})
	data, status, headers, err := c.call(ctx, http.MethodPost, c.sendURL, "Bearer "+credential.AccessToken, p.ClientID, string(body), "application/json")
	if err != nil {
		return "unknown", "", "", 0
	}
	switch status {
	case 400:
		return "failed", "", "twitch_bad_request", 0
	case 401:
		return "failed", "", "twitch_unauthorized", 0
	case 403:
		return "failed", "", "twitch_forbidden", 0
	case 422:
		return "failed", "", "twitch_unprocessable", 0
	case 429:
		delay := min(max(retryDelay(headers, time.Now()), time.Second), 5*time.Minute)
		return "failed", "", "twitch_rate_limited", int(delay.Seconds())
	case 200:
		var result struct {
			Data []struct {
				MessageID string `json:"message_id"`
				IsSent    *bool  `json:"is_sent"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &result) != nil || len(result.Data) != 1 || result.Data[0].IsSent == nil {
			return "unknown", "", "", 0
		}
		if !*result.Data[0].IsSent {
			return "failed", "", "twitch_rejected", 0
		}
		if safeMessageID(result.Data[0].MessageID) {
			return "sent", result.Data[0].MessageID, "", 0
		}
	}
	return "unknown", "", "", 0
}
