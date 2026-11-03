package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (g *graph) validateInboundSource(ctx context.Context, n node, m messageContent, owner int64) error {
	broadcaster, id, ok := inboundKey(n.Name)
	if !ok || m.Source == nil || m.Source.MessageID != id {
		return errCorrupt
	}
	configID, _ := resourceID(m.Source.ConfigID)
	rows, err := g.queryResourceItem(ctx, twitchConfigType, "", owner, g.configurationGroup(), configID)
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
	authorNode, authorErr := g.readEntity(ctx, mustResourceID(m.UserID), "user", owner)
	if authorErr != nil {
		return authorErr
	}
	var identity identityContent
	if authorNode.Content == nil || strictJSON([]byte(*authorNode.Content), &identity) != nil || identity.Service != "Twitch" || !positiveDecimal(identity.ServiceID) {
		return errCorrupt
	}
	if m.ReplyTo != nil && !m.Deleted {
		parent, err := g.readEntity(ctx, mustResourceID(*m.ReplyTo), "message", owner)
		if err != nil {
			return err
		}
		if parent.Name != twitchMessageName(broadcaster, m.Source.ParentMessageID) {
			return errCorrupt
		}
	}
	if p.BroadcasterUserID != broadcaster || p.ChannelID != m.ChannelID {
		return errCorrupt
	}
	return nil
}

func (g *graph) inboundLookup(ctx context.Context, name string, owner int64) (node, bool, error) {
	canonical, err := g.resourceNodes(ctx, "message", name, owner)
	if err != nil {
		return node{}, false, err
	}
	integrity, err := g.queryResourceNodes(ctx, "message", name, owner, 0)
	if err != nil {
		return node{}, false, err
	}
	if len(canonical) > 1 || len(integrity) > 1 || len(canonical) == 1 && (len(integrity) != 1 || !sameIdentityProjection(canonical[0], integrity[0])) {
		return node{}, false, errCorrupt
	}
	if len(integrity) == 0 {
		return node{}, false, nil
	}
	n := integrity[0]
	if n.Content != nil && *n.Content != "" {
		if len(canonical) != 1 {
			return node{}, false, errCorrupt
		}
		m, err := decodeMessage(n)
		if err != nil {
			return node{}, false, err
		}
		if err := g.validateRelations(ctx, n, m, owner); err != nil {
			return node{}, false, err
		}
	}
	return n, true, nil
}

func eligibleTwitch(cfg *authConfig, selected, id int64) (twitchProfile, bool) {
	for _, p := range cfg.twitch {
		grant, ok := cfg.Users[p.TenantUserID]
		if p.id == id && *p.Enabled && p.AuthConfigID == strconv.FormatInt(selected, 10) && ok && grant.Enabled && hasHost(grant.Permissions) {
			return p, true
		}
	}
	return twitchProfile{}, false
}

func sameReceiver(a, b twitchProfile) bool {
	return a.id == b.id && a.twitchTuple == b.twitchTuple && a.BotUserID == b.BotUserID && a.ClientID == b.ClientID && a.CredentialRef == b.CredentialRef
}

func (s *server) ingestTwitch(ctx context.Context, p twitchProfile, e twitchEvent) (int64, error) {
	if err := lockResources(ctx); err != nil {
		return 0, err
	}
	defer resourceLock.Unlock()
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return 0, err
	}
	current, ok := eligibleTwitch(cfg, s.graph.config, p.id)
	if !ok || !sameReceiver(current, p) {
		return 0, errUnavailable
	}
	raw, _ := json.Marshal(e)
	checked, err := parseTwitchEvent(raw, e.publishedAt, p)
	if err != nil {
		return 0, err
	}
	e = checked
	g, owner := s.graph, cfg.serviceOwner
	name := twitchMessageName(p.BroadcasterUserID, e.MessageID)
	n, exists, err := g.inboundLookup(ctx, name, owner)
	if err != nil {
		return 0, err
	}
	if exists && n.Content != nil && *n.Content != "" {
		m, err := decodeMessage(n)
		if err != nil || m.ChannelID != p.ChannelID {
			return 0, errCorrupt
		}
		originalID, _ := resourceID(m.Source.ConfigID)
		var original *twitchProfile
		for i := range cfg.twitch {
			if cfg.twitch[i].id == originalID {
				original = &cfg.twitch[i]
			}
		}
		if original == nil || original.twitchTuple != p.twitchTuple {
			return 0, errCorrupt
		}
		authorNode, err := g.readEntity(ctx, mustResourceID(m.UserID), "user", owner)
		if err != nil {
			return 0, err
		}
		if _, err := identityValue(authorNode, "user", "Twitch", e.ChatterUserID); err != nil {
			return 0, err
		}
		return n.ID, nil
	}
	if exists {
		if err := g.validateInboundStaging(ctx, n, p, e, owner); err != nil {
			return 0, err
		}
	}
	uid, err := g.resolveIdentity(ctx, "user", identityContent{SchemaVersion: 1, Service: "Twitch", ServiceID: e.ChatterUserID, Name: e.ChatterUserName}, owner)
	if err != nil {
		return 0, err
	}
	cid, err := g.resolveIdentity(ctx, "channel", identityContent{SchemaVersion: 1, Service: "Twitch", Channel: p.BroadcasterUserID}, owner)
	if err != nil {
		return 0, err
	}
	if strconv.FormatInt(cid, 10) != p.ChannelID {
		return 0, errCorrupt
	}
	source := &twitchSource{Kind: "twitch", ConfigID: strconv.FormatInt(p.id, 10), MessageID: e.MessageID, PublishedAt: &e.publishedAt, BroadcasterLogin: e.BroadcasterUserLogin, ChatterLogin: e.ChatterUserLogin, SourceBroadcasterUserID: e.SourceBroadcasterUserID, SourceMessageID: e.SourceMessageID}
	if e.Reply != nil {
		source.ParentMessageID = e.Reply.ParentMessageID
	}
	now := s.now().UTC()
	m := messageContent{SchemaVersion: 2, Source: source, UserID: strconv.FormatInt(uid, 10), ChannelID: p.ChannelID, Time: now, Service: "Twitch", Channel: p.BroadcasterUserID, User: &author{Service: "Twitch", ServiceID: e.ChatterUserID, Name: e.ChatterUserName}, Chunks: []chunk{{Type: 0, Data: e.Message.Text}}, TTS: new(false)}
	if source.ParentMessageID != "" {
		parent, found, err := g.inboundLookup(ctx, twitchMessageName(p.BroadcasterUserID, source.ParentMessageID), owner)
		if err != nil {
			return 0, err
		}
		if found && parent.Content != nil && *parent.Content != "" {
			pm, err := decodeMessage(parent)
			if err != nil {
				return 0, err
			}
			if !pm.Deleted && pm.ChannelID == p.ChannelID && (!exists || parent.ID < n.ID) {
				v := strconv.FormatInt(parent.ID, 10)
				m.ReplyTo = &v
			}
		}
	}
	if !exists {
		metadata, _ := json.Marshal(map[string]any{"name": name, "type": "message", "rootNodeId": g.root, "access": 0, "skipEmbedding": true})
		_, createErr := g.request(ctx, http.MethodPost, "/nodes", nil, metadata)
		n, exists, err = g.inboundLookup(ctx, name, owner)
		if err != nil || !exists {
			return 0, errOutcomeUnknown
		}
		if createErr != nil && n.Content != nil && *n.Content != "" {
			return 0, errCorrupt
		}
	}
	expected := []relation{{Source: n.ID, Target: g.group, Type: "None"}, {Source: n.ID, Target: uid, Type: "Unidirectional", Context: "authored-by"}, {Source: n.ID, Target: cid, Type: "Unidirectional", Context: "in-channel"}}
	if m.ReplyTo != nil {
		id, _ := resourceID(*m.ReplyTo)
		if id >= n.ID {
			return 0, errCorrupt
		}
		expected = append(expected, relation{Source: n.ID, Target: id, Type: "Unidirectional", Context: "replies-to"})
	}
	if n.Content != nil && *n.Content != "" {
		return 0, errCorrupt
	}
	for _, edge := range n.Links {
		found := false
		for _, want := range expected {
			if edge == want || want.Type == "None" && edge.Type == "None" && edge.Context == "" && edge.Source == want.Target && edge.Target == want.Source {
				found = true
			}
		}
		if !found {
			return 0, errCorrupt
		}
	}
	for _, want := range expected {
		count := 0
		for _, edge := range n.Links {
			if edge == want || want.Type == "None" && edge.Type == "None" && edge.Context == "" && edge.Source == want.Target && edge.Target == want.Source {
				count++
			}
		}
		if count > 1 {
			return 0, errCorrupt
		}
		if count == 0 {
			body, _ := json.Marshal(want.Target)
			_, writeErr := g.request(ctx, http.MethodPost, nodePath(n.ID)+"/links", url.Values{"linkType": {want.Type}, "context": {want.Context}}, body)
			next, found, err := g.inboundLookup(ctx, name, owner)
			if err != nil || !found {
				return 0, errOutcomeUnknown
			}
			confirmed := false
			for _, edge := range next.Links {
				if edge == want {
					confirmed = true
				}
			}
			if !confirmed {
				if writeErr != nil {
					return 0, writeErr
				}
				return 0, errCorrupt
			}
			n = next
		}
	}
	confirmed, err := g.readEntity(ctx, n.ID, "message", owner)
	if err != nil {
		return 0, err
	}
	if err := g.validateRelations(ctx, confirmed, m, owner); err != nil {
		return 0, err
	}
	writeErr := g.replaceContent(ctx, n.ID, m)
	read, found, err := g.inboundLookup(ctx, name, owner)
	if err != nil || !found || read.Content == nil || *read.Content == "" {
		if writeErr != nil {
			return 0, errOutcomeUnknown
		}
		return 0, errCorrupt
	}
	stored, err := decodeMessage(read)
	if err != nil || !stored.Time.Equal(m.Time) || stored.Source.MessageID != e.MessageID {
		return 0, errCorrupt
	}
	return read.ID, nil
}

func inboundDeadline(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 10*time.Second)
}

func mustResourceID(value string) int64 { id, _ := resourceID(value); return id }
