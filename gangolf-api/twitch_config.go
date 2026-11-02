package main

import (
	"context"
	"mime"
	"strconv"
	"strings"
)

const twitchConfigType = "gangolf-twitch-config"

type twitchTuple struct {
	AuthConfigID      string `json:"authConfigId"`
	TenantUserID      string `json:"tenantUserId"`
	ChannelID         string `json:"channelId"`
	BroadcasterUserID string `json:"broadcasterUserId"`
}

type twitchProfile struct {
	twitchTuple
	SchemaVersion int    `json:"schemaVersion"`
	BotUserID     string `json:"botUserId"`
	ClientID      string `json:"clientId"`
	CredentialRef string `json:"credentialRef"`
	Enabled       *bool  `json:"enabled"`
	id            int64
	name          string
}

func safeSlug(s string) bool {
	if len(s) == 0 || len(s) > 128 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func positiveDecimal(s string) bool {
	_, ok := resourceID(s)
	return ok
}

func decodeTwitchProfile(n node) (twitchProfile, error) {
	var p twitchProfile
	media, _, err := mime.ParseMediaType(n.ContentType)
	if err != nil || media != "application/json" || n.Content == nil || strictJSON([]byte(*n.Content), &p) != nil || p.SchemaVersion != 1 || p.Enabled == nil || *p.Enabled || n.SkipEmbedding == nil || !*n.SkipEmbedding {
		return p, errCorrupt
	}
	for _, id := range []string{p.AuthConfigID, p.TenantUserID, p.ChannelID, p.BroadcasterUserID, p.BotUserID} {
		if !positiveDecimal(id) {
			return p, errCorrupt
		}
	}
	if len(p.ClientID) == 0 || len(p.ClientID) > 128 {
		return p, errCorrupt
	}
	for _, c := range p.ClientID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return p, errCorrupt
		}
	}
	if !safeSlug(p.CredentialRef) {
		return p, errCorrupt
	}
	parts := strings.Split(strings.TrimPrefix(n.Name, "Twitch inbound — "), " — ")
	if !strings.HasPrefix(n.Name, "Twitch inbound — ") || len(parts) != 3 || !safeSlug(parts[0]) || parts[1] != "user-"+p.TenantUserID || !safeSlug(parts[2]) {
		return p, errCorrupt
	}
	p.id, p.name = n.ID, n.Name
	return p, nil
}

func (g *graph) configurationGroup() int64 {
	g.groupMu.RLock()
	defer g.groupMu.RUnlock()
	return g.groups["configuration"]
}

func (g *graph) validateTwitchProfile(ctx context.Context, n node, owner int64) (twitchProfile, error) {
	p, err := decodeTwitchProfile(n)
	if err != nil {
		return p, err
	}
	authID, _ := resourceID(p.AuthConfigID)
	channelID, _ := resourceID(p.ChannelID)
	expected := map[string]int64{"configured-for": channelID, "uses-auth-config": authID}
	for _, e := range n.Links {
		if e.Type == "None" && e.Context == "" {
			continue
		}
		target, ok := expected[e.Context]
		if !ok || e.Source != n.ID || e.Target != target || e.Type != "Unidirectional" {
			return p, errCorrupt
		}
		delete(expected, e.Context)
	}
	if len(expected) != 0 {
		return p, errCorrupt
	}
	for _, ref := range []struct {
		kind      string
		id, group int64
		relation  string
	}{{authConfigType, authID, g.configurationGroup(), "uses-auth-config"}, {"channel", channelID, g.groupsChannel(), "configured-for"}} {
		nodes, err := g.queryResourceItem(ctx, ref.kind, "", owner, ref.group, ref.id)
		if err != nil {
			return p, err
		}
		if len(nodes) != 1 {
			return p, errCorrupt
		}
		target := nodes[0]
		found := 0
		for _, e := range target.Links {
			if e.Source == n.ID && e.Target == target.ID && e.Type == "Unidirectional" && e.Context == ref.relation {
				found++
			}
		}
		if found != 1 {
			return p, errCorrupt
		}
		if ref.kind == "channel" {
			media, _, err := mime.ParseMediaType(target.ContentType)
			if err != nil || media != "application/json" {
				return p, errCorrupt
			}
			if _, err := identityValue(target, "channel", "Twitch", p.BroadcasterUserID); err != nil {
				return p, err
			}
			channels, err := g.resourceNodes(ctx, "channel", target.Name, owner)
			if err != nil {
				return p, err
			}
			if len(channels) != 1 || channels[0].ID != target.ID {
				return p, errCorrupt
			}
		} else if err := g.validateAuthProfileReference(ctx, target, owner); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (g *graph) validateAuthProfileReference(ctx context.Context, n node, owner int64) error {
	if n.Type != authConfigType || !g.privateNode(n, owner) || n.SkipEmbedding == nil || !*n.SkipEmbedding || !authProfileName(n.Name) {
		return errCorrupt
	}
	canonical, err := g.queryResourceNodes(ctx, authConfigType, n.Name, owner, g.configurationGroup())
	if err != nil {
		return err
	}
	if len(canonical) != 1 || canonical[0].ID != n.ID || !sameIdentityProjection(n, canonical[0]) {
		return errCorrupt
	}
	integrity, err := g.queryResourceNodes(ctx, authConfigType, n.Name, owner, 0)
	if err != nil {
		return err
	}
	if len(integrity) != 1 || integrity[0].ID != n.ID || !sameIdentityProjection(n, integrity[0]) {
		return errCorrupt
	}
	media, _, err := mime.ParseMediaType(n.ContentType)
	var cfg authConfig
	if err != nil || media != "application/json" || n.Content == nil || strictJSON([]byte(*n.Content), &cfg) != nil || !cfg.valid() {
		return errCorrupt
	}
	return nil
}

func (g *graph) groupsChannel() int64 { id, _ := g.groupID("channel"); return id }

func (g *graph) validateConfigIncoming(ctx context.Context, n node, owner int64) error {
	for _, e := range n.Links {
		if e.Context != "configured-for" && e.Context != "uses-auth-config" {
			continue
		}
		if e.Target != n.ID || e.Type != "Unidirectional" || n.Type == "channel" && e.Context != "configured-for" || n.Type == authConfigType && e.Context != "uses-auth-config" {
			return errCorrupt
		}
		rows, err := g.queryResourceItem(ctx, twitchConfigType, "", owner, g.configurationGroup(), e.Source)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return errCorrupt
		}
		p, err := g.validateTwitchProfile(ctx, rows[0], owner)
		if err != nil {
			return err
		}
		expected := p.ChannelID
		if n.Type == authConfigType {
			expected = p.AuthConfigID
		}
		if expected != strconv.FormatInt(n.ID, 10) {
			return errCorrupt
		}
	}
	return nil
}

func (g *graph) loadTwitchProfiles(ctx context.Context, owner int64, auth node) ([]twitchProfile, error) {
	rows, err := g.queryResourceNodes(ctx, twitchConfigType, "", owner, g.configurationGroup())
	if err != nil {
		return nil, err
	}
	profiles := make([]twitchProfile, 0, len(rows))
	names := map[string]bool{}
	broadcasters := map[string]twitchProfile{}
	tuples := map[int64]twitchTuple{}
	for _, n := range rows {
		p, err := g.validateTwitchProfile(ctx, n, owner)
		if err != nil {
			return nil, err
		}
		channelID, _ := resourceID(p.ChannelID)
		if _, err := g.readEntity(ctx, channelID, "channel", owner); err != nil {
			return nil, err
		}
		authID, _ := resourceID(p.AuthConfigID)
		authRows, err := g.queryResourceItem(ctx, authConfigType, "", owner, g.configurationGroup(), authID)
		if err != nil {
			return nil, err
		}
		if len(authRows) != 1 {
			return nil, errCorrupt
		}
		if err := g.validateConfigIncoming(ctx, authRows[0], owner); err != nil {
			return nil, err
		}
		if names[n.Name] {
			return nil, errCorrupt
		}
		names[n.Name] = true
		named, err := g.queryResourceNodes(ctx, twitchConfigType, n.Name, owner, 0)
		if err != nil {
			return nil, err
		}
		if len(named) != 1 || !sameIdentityProjection(n, named[0]) {
			return nil, errCorrupt
		}
		if old, ok := broadcasters[p.BroadcasterUserID]; ok {
			if old.twitchTuple != p.twitchTuple || *old.Enabled && *p.Enabled {
				return nil, errCorrupt
			}
		}
		if old, ok := broadcasters[p.BroadcasterUserID]; !ok || !*old.Enabled {
			broadcasters[p.BroadcasterUserID] = p
		}
		tuples[p.id] = p.twitchTuple
		profiles = append(profiles, p)
	}
	if err := g.validateConfigIncoming(ctx, auth, owner); err != nil {
		return nil, err
	}
	g.twitchMu.Lock()
	defer g.twitchMu.Unlock()
	for id, old := range g.twitchTuples {
		if current, ok := tuples[id]; !ok || current != old {
			return nil, errCorrupt
		}
	}
	g.twitchTuples = tuples
	return profiles, nil
}
