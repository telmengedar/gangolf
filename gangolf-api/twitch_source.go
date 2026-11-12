package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

type twitchSource struct {
	Transport               string     `json:"transport,omitempty"`
	Kind                    string     `json:"kind"`
	ConfigID                string     `json:"configId"`
	MessageID               string     `json:"messageId"`
	PublishedAt             *time.Time `json:"publishedAt,omitempty"`
	BroadcasterLogin        string     `json:"broadcasterLogin,omitempty"`
	ChatterLogin            string     `json:"chatterLogin,omitempty"`
	ParentMessageID         string     `json:"parentMessageId,omitempty"`
	SourceBroadcasterUserID string     `json:"sourceBroadcasterUserId,omitempty"`
	SourceMessageID         string     `json:"sourceMessageId,omitempty"`
}

func (s *twitchSource) tombstone() *twitchSource {
	if s == nil {
		return nil
	}
	return &twitchSource{Kind: s.Kind, ConfigID: s.ConfigID, MessageID: s.MessageID}
}

func twitchMessageName(broadcaster, messageID string) string {
	data, _ := json.Marshal([]string{broadcaster, messageID})
	return "gangolf-twitch-message:" + base64.RawURLEncoding.EncodeToString(data)
}

func inboundKey(name string) (string, string, bool) {
	raw, ok := strings.CutPrefix(name, "gangolf-twitch-message:")
	data, err := base64.RawURLEncoding.DecodeString(raw)
	var tuple []string
	if !ok || err != nil || json.Unmarshal(data, &tuple) != nil || len(tuple) != 2 || !positiveDecimal(tuple[0]) || !safeMessageID(tuple[1]) || twitchMessageName(tuple[0], tuple[1]) != name {
		return "", "", false
	}
	return tuple[0], tuple[1], true
}

func safeMessageID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validInboundName(name string) bool { _, _, ok := inboundKey(name); return ok }

func validMessageName(name string, m messageContent) bool {
	if m.SchemaVersion == 3 || m.SchemaVersion == 4 {
		return validOutboundContent(name, m)
	}
	if m.Delivery != nil {
		return false
	}
	if m.SchemaVersion == 1 {
		return name == "message" && m.Source == nil
	}
	broadcaster, id, ok := inboundKey(name)
	s := m.Source
	if !ok || s == nil || s.Kind != "twitch" || (s.Transport != "" && s.Transport != "irc") || !positiveDecimal(s.ConfigID) || s.MessageID != id {
		return false
	}
	if m.Deleted {
		return s.Transport == "" && s.PublishedAt == nil && s.BroadcasterLogin == "" && s.ChatterLogin == "" && s.ParentMessageID == "" && s.SourceBroadcasterUserID == "" && s.SourceMessageID == ""
	}
	return m.Service == "Twitch" && m.Channel == broadcaster && m.User != nil && m.User.Service == "Twitch" && positiveDecimal(m.User.ServiceID) && s.PublishedAt != nil && !s.PublishedAt.IsZero() && s.PublishedAt.Location() == time.UTC && s.BroadcasterLogin != "" && s.ChatterLogin != "" && (s.ParentMessageID == "" || safeMessageID(s.ParentMessageID)) && ((s.SourceBroadcasterUserID == "" && s.SourceMessageID == "") || (positiveDecimal(s.SourceBroadcasterUserID) && safeMessageID(s.SourceMessageID)))
}
