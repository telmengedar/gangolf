package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type twitchDelivery struct {
	RequestKey string `json:"requestKey"`
	AuthUserID string `json:"authUserId"`
	ConfigID   string `json:"configId"`
	State      string `json:"state"`
	MessageID  string `json:"messageId,omitempty"`
	Code       string `json:"code,omitempty"`
}

func validRequestKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func outboundName(authID, userID, key string) string {
	data, _ := json.Marshal([]string{authID, userID, key})
	return "gangolf-twitch-outbound:" + base64.RawURLEncoding.EncodeToString(data)
}

func outboundKey(name string) (string, string, string, bool) {
	raw, ok := strings.CutPrefix(name, "gangolf-twitch-outbound:")
	data, err := base64.RawURLEncoding.DecodeString(raw)
	var tuple []string
	if !ok || err != nil || json.Unmarshal(data, &tuple) != nil || len(tuple) != 3 || !positiveDecimal(tuple[0]) || tuple[1] == "" || !validRequestKey(tuple[2]) || outboundName(tuple[0], tuple[1], tuple[2]) != name {
		return "", "", "", false
	}
	return tuple[0], tuple[1], tuple[2], true
}

func validOutboundName(name string) bool {
	_, _, _, ok := outboundKey(name)
	return ok
}

func outboundText(input messageInput) (string, string) {
	if input.Service != "Twitch" || !positiveDecimal(input.Channel) || input.User != (author{Service: "Twitch", ServiceID: "GangolfBot", Name: "GangolfBot"}) || input.TTS || input.ReplyTo != nil || nonNull(input.PostTo) || len(input.Chunks) == 0 {
		return "", "delivery_not_supported"
	}
	var text strings.Builder
	for _, c := range input.Chunks {
		if c.Type != 0 || c.Width != nil && *c.Width < 0 || c.Height != nil && *c.Height < 0 {
			return "", "delivery_not_supported"
		}
		text.WriteString(c.Data)
	}
	value := text.String()
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", "delivery_not_supported"
	}
	if utf8.RuneCountInString(value) > 500 {
		return "", "message_too_large"
	}
	return value, ""
}

func validOutboundContent(name string, m messageContent) bool {
	_, userID, key, ok := outboundKey(name)
	d := m.Delivery
	if !ok || m.Source != nil || d == nil || d.AuthUserID != userID || d.RequestKey != key || !positiveDecimal(d.ConfigID) || m.ReplyTo != nil {
		return false
	}
	switch d.State {
	case "unknown":
		if d.MessageID != "" || d.Code != "" {
			return false
		}
	case "sent":
		if !safeMessageID(d.MessageID) || d.Code != "" {
			return false
		}
	case "failed":
		if d.MessageID != "" || !validDeliveryCode(d.Code) {
			return false
		}
	default:
		return false
	}
	if m.Deleted {
		return true
	}
	if m.User == nil || m.TTS == nil {
		return false
	}
	_, code := outboundText(messageInput{Service: m.Service, Channel: m.Channel, User: *m.User, Chunks: m.Chunks, TTS: *m.TTS, ReplyTo: m.ReplyTo})
	return code == ""
}

func validDeliveryCode(code string) bool {
	switch code {
	case "twitch_rejected", "twitch_bad_request", "twitch_unauthorized", "twitch_forbidden", "twitch_unprocessable", "twitch_rate_limited":
		return true
	default:
		return false
	}
}
