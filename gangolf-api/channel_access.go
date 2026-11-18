package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

func (g *graph) validChannelNode(ctx context.Context, n node, owner int64) bool {
	if n.ID <= 0 || n.Owner != owner || !n.privateAccess() {
		return false
	}
	if n.Root == g.root {
		return true
	}
	if n.SkipEmbedding == nil || !*n.SkipEmbedding {
		return false
	}
	_, profile, err := g.appProfile(ctx, n.Root, owner)
	return err == nil && positiveDecimal(profile.UserID)
}

func channelIdentity(n node) (identityContent, error) {
	var identity identityContent
	if n.Content == nil || strictJSON([]byte(*n.Content), &identity) != nil {
		return identity, errCorrupt
	}
	_, err := identityValue(n, "channel", identity.Service, identity.Channel)
	return identity, err
}

func (p messagePolicy) channel(ctx context.Context, service, channel, id string) (*node, error) {
	key := id
	if key == "" {
		key = identityName("channel", service, channel)
	}
	if n, ok := p.channels[key]; ok {
		return n, nil
	}
	var rows []node
	var err error
	if id != "" {
		parsed, ok := resourceID(id)
		if !ok {
			return nil, errCorrupt
		}
		rows, err = p.graph.queryResourceItem(ctx, "channel", "", p.cfg.serviceOwner, p.graph.groupsChannel(), parsed)
	} else {
		rows, err = p.graph.resourceNodes(ctx, "channel", key, p.cfg.serviceOwner)
	}
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, errCorrupt
	}
	if len(rows) == 0 {
		p.channels[key] = nil
		return nil, nil
	}
	n := rows[0]
	if n.Root != p.graph.root {
		_, profile, err := p.graph.appProfile(ctx, n.Root, p.cfg.serviceOwner)
		owner, valid := p.channelOwner(n)
		if err != nil || !valid || profile.UserID != owner {
			return nil, errCorrupt
		}
	}
	if n.Content != nil && *n.Content != "" {
		v, err := channelIdentity(n)
		if err != nil {
			return nil, err
		}
		if service != "" && v.Service != service || channel != "" && v.Channel != channel {
			return nil, nil
		}
	} else if n.Root != p.graph.root {
		return nil, errCorrupt
	}
	integrity, err := p.graph.identityIntegrity(ctx, "channel", n.Name, p.cfg.serviceOwner)
	if err != nil || len(integrity) != 1 || !sameIdentityProjection(n, integrity[0]) {
		return nil, errCorrupt
	}
	p.channels[key] = &n
	p.channels[strconv.FormatInt(n.ID, 10)] = &n
	p.channels[n.Name] = &n
	return &n, nil
}

func (p messagePolicy) channelOwner(n node) (string, bool) {
	if n.Root == p.graph.root {
		return "", true
	}
	owner, ok := p.owners[n.Root]
	return owner, ok
}

func (p messagePolicy) allowed(ctx context.Context, service, channel, id string, active bool) (bool, error) {
	n, err := p.channel(ctx, service, channel, id)
	if err != nil {
		return false, err
	}
	if n == nil {
		if id != "" {
			return false, nil
		}
		if p.principal.Scope == "channel-read" || p.principal.ManagedOnly && !p.principal.admin() {
			return false, nil
		}
		return p.visible(service, channel, id), nil
	}
	if (n.Content == nil || *n.Content == "") && n.Root == p.graph.root && !p.principal.ManagedOnly && p.principal.Scope != "channel-read" {
		return p.visible(service, channel, id), nil
	}
	identity, err := channelIdentity(*n)
	if err != nil {
		return false, err
	}
	if service != "" && identity.Service != service || channel != "" && identity.Channel != channel {
		return false, nil
	}
	if p.principal.Scope == "channel-read" {
		if !permissionsContain(p.principal.ChannelIDs, strconv.FormatInt(n.ID, 10)) {
			return false, nil
		}
		return p.scopedReadChannel(ctx, strconv.FormatInt(n.ID, 10), active)
	}
	owner, valid := p.channelOwner(*n)
	if !valid {
		return false, errCorrupt
	}
	if owner != "" {
		if identity.SchemaVersion != 2 {
			return false, errCorrupt
		}
		if active && (identity.Enabled == nil || !*identity.Enabled || identity.Deleted) {
			return false, nil
		}
		user := p.cfg.Users[owner]
		if user.ProfileNodeID == "" {
			return false, errCorrupt
		}
		if !p.principal.admin() && owner != p.principal.UserID {
			return false, nil
		}
	} else {
		if identity.SchemaVersion != 1 {
			return false, errCorrupt
		}
		if p.principal.ManagedOnly && !p.principal.admin() {
			return false, nil
		}
		if !p.visible(service, channel, strconv.FormatInt(n.ID, 10)) {
			return false, nil
		}
	}
	return true, nil
}

func (p messagePolicy) scopedReadChannel(ctx context.Context, id string, active bool) (bool, error) {
	n, err := p.channel(ctx, "", "", id)
	if err != nil || n == nil {
		return false, err
	}
	identity, err := channelIdentity(*n)
	if err != nil {
		return false, err
	}
	owner, valid := p.channelOwner(*n)
	if !valid {
		return false, errCorrupt
	}
	if owner != "" {
		if identity.SchemaVersion != 2 {
			return false, errCorrupt
		}
		return owner == p.principal.UserID && (!active || identity.Enabled != nil && *identity.Enabled && !identity.Deleted), nil
	}
	if identity.SchemaVersion != 1 || identity.Service != "Twitch" {
		return false, nil
	}
	matches := 0
	for _, profile := range p.profiles {
		if profile.AuthConfigID == p.authID && profile.TenantUserID == p.principal.UserID && profile.ChannelID == id && profile.BroadcasterUserID == identity.Channel && profile.Enabled != nil && *profile.Enabled {
			matches++
		}
	}
	return matches == 1, nil
}

func (s *server) twitchChannelAllowed(ctx context.Context, cfg *authConfig, profile twitchProfile) error {
	id, ok := resourceID(profile.ChannelID)
	if !ok {
		return errCorrupt
	}
	n, err := s.graph.readEntity(ctx, id, "channel", cfg.serviceOwner)
	if err != nil {
		return err
	}
	identity, err := channelIdentity(n)
	if err != nil {
		return err
	}
	if n.Root == s.graph.root {
		return nil
	}
	policy := s.policy(principal{UserID: profile.TenantUserID}, cfg)
	owner, valid := policy.channelOwner(n)
	_, app, profileErr := s.graph.appProfile(ctx, n.Root, cfg.serviceOwner)
	if profileErr != nil || app.UserID != owner {
		return errCorrupt
	}
	if !valid || owner != profile.TenantUserID || identity.Enabled == nil || !*identity.Enabled || identity.Deleted || !strings.EqualFold(identity.Service, "Twitch") || identity.Channel != profile.BroadcasterUserID {
		return errUnavailable
	}
	return nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
