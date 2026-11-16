package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type principal struct {
	UserID      string
	Permissions []string
	Scope       string
	ChannelIDs  []string
	KeyID       string
	ManagedOnly bool
	ExpiresAt   *time.Time
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
	cfg       *authConfig
	graph     *graph
	channels  map[string]*node
	owners    map[int64]string
}

func (s *server) policy(p principal, cfg *authConfig) messagePolicy {
	owners := make(map[int64]string)
	for id, user := range cfg.Users {
		if user.ProfileNodeID != "" {
			nodeID, _ := resourceID(user.ProfileNodeID)
			owners[nodeID] = id
		}
	}
	return messagePolicy{principal: p, authID: strconv.FormatInt(s.graph.config, 10), profiles: cfg.twitch, cfg: cfg, graph: s.graph, channels: make(map[string]*node), owners: owners}
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
		return p.allowed(ctx, m.Service, m.Channel, m.ChannelID, false)
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
	return p.allowed(ctx, identity.Service, identity.Channel, m.ChannelID, false)
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
	if err := lockResources(ctx); err != nil {
		storageFailure(w, err)
		return
	}
	defer resourceLock.Unlock()
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
		code := ""
		var dropped uint64
		if *profile.Enabled && s.twitch != nil {
			state, code, dropped = s.twitch.status(profile.id)
		}
		entry := map[string]any{"errorCode": code, "dropped": dropped, "configId": strconv.FormatInt(profile.id, 10), "name": profile.name, "channelId": profile.ChannelID, "broadcasterUserId": profile.BroadcasterUserID, "enabled": *profile.Enabled, "state": state}
		if profile.Transport != "" {
			entry["transport"] = profile.Transport
		}
		result = append(result, entry)
	}
	respond(w, 200, map[string]any{"result": result})
}
