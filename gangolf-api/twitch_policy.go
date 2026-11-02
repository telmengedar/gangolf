package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

type principal struct {
	UserID      string
	Permissions []string
}

func (p principal) admin() bool {
	for _, permission := range p.Permissions {
		if permission == "admin" {
			return true
		}
	}
	return false
}

type messagePolicy struct {
	principal principal
	authID    string
	profiles  []twitchProfile
}

func (s *server) policy(p principal, cfg *authConfig) messagePolicy {
	return messagePolicy{principal: p, authID: strconv.FormatInt(s.graph.config, 10), profiles: cfg.twitch}
}

func (p messagePolicy) visible(service, channel, channelID string) bool {
	if p.principal.admin() || !strings.EqualFold(service, "Twitch") {
		return true
	}
	for _, profile := range p.profiles {
		if profile.AuthConfigID == p.authID && profile.TenantUserID == p.principal.UserID && profile.BroadcasterUserID == channel && (channelID == "" || profile.ChannelID == channelID) {
			return true
		}
	}
	return false
}

func (p messagePolicy) visibleV3(ctx context.Context, g *graph, m messageContent, owner int64) (bool, error) {
	if !m.Deleted {
		return p.visible(m.Service, m.Channel, m.ChannelID), nil
	}
	id, _ := resourceID(m.ChannelID)
	n, err := g.readEntity(ctx, id, "channel", owner)
	if err != nil {
		return false, err
	}
	var identity identityContent
	if n.Content == nil || strictJSON([]byte(*n.Content), &identity) != nil {
		return false, errCorrupt
	}
	return p.visible(identity.Service, identity.Channel, m.ChannelID), nil
}

func (s *server) twitchChannels(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		failure(w, 405, "method_not_allowed")
		return
	}
	token, query := credential(r)
	if token == "" || query {
		failure(w, 401, "unauthorized")
		return
	}
	if r.URL.RawQuery != "" {
		failure(w, 400, "invalid_filter")
		return
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		storageFailure(w, err)
		return
	}
	p, status := s.authorize(ctx, r, cfg, token, false)
	if status != 0 {
		code := "unauthorized"
		if status == 403 {
			code = "forbidden"
		} else if status == 503 {
			code = "auth_unavailable"
		}
		failure(w, status, code)
		return
	}
	result := make([]map[string]any, 0)
	for _, profile := range cfg.twitch {
		if !p.admin() && (profile.AuthConfigID != strconv.FormatInt(s.graph.config, 10) || profile.TenantUserID != p.UserID) {
			continue
		}
		state := "disabled"
		if *profile.Enabled {
			state = "auth-required"
		}
		result = append(result, map[string]any{"configId": strconv.FormatInt(profile.id, 10), "name": profile.name, "channelId": profile.ChannelID, "broadcasterUserId": profile.BroadcasterUserID, "enabled": *profile.Enabled, "state": state})
	}
	respond(w, 200, map[string]any{"result": result})
}
