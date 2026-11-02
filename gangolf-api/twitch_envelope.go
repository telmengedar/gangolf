package main

import "encoding/json"

type twitchEnvelope struct {
	Metadata struct {
		MessageID           string `json:"message_id"`
		MessageType         string `json:"message_type"`
		MessageTimestamp    string `json:"message_timestamp"`
		SubscriptionType    string `json:"subscription_type"`
		SubscriptionVersion string `json:"subscription_version"`
	} `json:"metadata"`
	Payload struct {
		Session struct {
			ID                      string `json:"id"`
			Status                  string `json:"status"`
			KeepaliveTimeoutSeconds int    `json:"keepalive_timeout_seconds"`
			ReconnectURL            string `json:"reconnect_url"`
		} `json:"session"`
		Subscription twitchSubscription `json:"subscription"`
		Event        json.RawMessage    `json:"event"`
	} `json:"payload"`
}
