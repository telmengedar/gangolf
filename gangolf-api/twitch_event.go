package main

import (
	"encoding/json"
	"strings"
	"time"
)

type twitchEvent struct {
	BroadcasterUserID    string `json:"broadcaster_user_id"`
	BroadcasterUserLogin string `json:"broadcaster_user_login"`
	ChatterUserID        string `json:"chatter_user_id"`
	ChatterUserLogin     string `json:"chatter_user_login"`
	ChatterUserName      string `json:"chatter_user_name"`
	MessageID            string `json:"message_id"`
	Message              struct {
		Text string `json:"text"`
	} `json:"message"`
	Reply *struct {
		ParentMessageID string `json:"parent_message_id"`
	} `json:"reply"`
	SourceBroadcasterUserID string `json:"source_broadcaster_user_id"`
	SourceMessageID         string `json:"source_message_id"`
	publishedAt             time.Time
}

func parseTwitchEvent(raw json.RawMessage, published time.Time, p twitchProfile) (twitchEvent, error) {
	var e twitchEvent
	if json.Unmarshal(raw, &e) != nil || e.BroadcasterUserID != p.BroadcasterUserID || !positiveDecimal(e.ChatterUserID) || e.BroadcasterUserLogin == "" || e.ChatterUserLogin == "" || strings.TrimSpace(e.ChatterUserName) == "" || !safeMessageID(e.MessageID) || e.Message.Text == "" || published.IsZero() || e.Reply != nil && !safeMessageID(e.Reply.ParentMessageID) || (e.SourceBroadcasterUserID != "" || e.SourceMessageID != "") && (!positiveDecimal(e.SourceBroadcasterUserID) || !safeMessageID(e.SourceMessageID)) {
		return e, errCorrupt
	}
	e.publishedAt = published.UTC()
	return e, nil
}
