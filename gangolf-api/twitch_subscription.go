package main

type twitchSubscription struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Type      string `json:"type"`
	Version   string `json:"version"`
	Condition struct {
		BroadcasterUserID string `json:"broadcaster_user_id"`
		UserID            string `json:"user_id"`
	} `json:"condition"`
	Transport struct {
		Method    string `json:"method"`
		SessionID string `json:"session_id"`
	} `json:"transport"`
}

func (s twitchSubscription) matches(p twitchProfile, session, id string) bool {
	return s.ID != "" && (id == "" || id == s.ID) && s.Status == "enabled" && s.Type == "channel.chat.message" && s.Version == "1" && s.Condition.BroadcasterUserID == p.BroadcasterUserID && s.Condition.UserID == p.BotUserID && s.Transport.Method == "websocket" && s.Transport.SessionID == session
}
