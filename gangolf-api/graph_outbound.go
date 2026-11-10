package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
)

func (g *graph) validateOutbound(ctx context.Context, n node, m messageContent, owner int64) error {
	authID, _, _, ok := outboundKey(n.Name)
	if !ok || !validOutboundContent(n.Name, m) || n.SkipEmbedding == nil || !*n.SkipEmbedding {
		return errCorrupt
	}
	rows, err := g.queryResourceItem(ctx, twitchConfigType, "", owner, g.configurationGroup(), mustResourceID(m.Delivery.ConfigID))
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
	if p.AuthConfigID != authID || p.ChannelID != m.ChannelID || !m.Deleted && p.BroadcasterUserID != m.Channel {
		return errCorrupt
	}
	user, err := g.readEntity(ctx, mustResourceID(m.UserID), "user", owner)
	if err != nil {
		return err
	}
	if _, err := identityValue(user, "user", "Twitch", "GangolfBot"); err != nil {
		return err
	}
	return nil
}

func (g *graph) outboundLookup(ctx context.Context, name string, owner int64) (node, messageContent, bool, error) {
	canonical, err := g.resourceNodes(ctx, "message", name, owner)
	if err != nil {
		return node{}, messageContent{}, false, err
	}
	integrity, err := g.queryResourceNodes(ctx, "message", name, owner, 0)
	if err != nil {
		return node{}, messageContent{}, false, err
	}
	if len(canonical) == 0 && len(integrity) == 0 {
		return node{}, messageContent{}, false, nil
	}
	if len(canonical) != 1 || len(integrity) != 1 || !sameIdentityProjection(canonical[0], integrity[0]) {
		return node{}, messageContent{}, false, errCorrupt
	}
	n := canonical[0]
	if n.Content == nil || *n.Content == "" {
		return n, messageContent{}, false, errCorrupt
	}
	m, err := decodeMessage(n)
	if err == nil {
		err = g.validateRelations(ctx, n, m, owner)
	}
	return n, m, err == nil, err
}

func (g *graph) reserveOutbound(ctx context.Context, name string, m messageContent, owner int64) (int64, error) {
	n, err := g.createResource(ctx, "message", name, owner)
	if err != nil {
		return n.ID, errOutcomeUnknown
	}
	for _, edge := range []relation{{Source: n.ID, Target: mustResourceID(m.UserID), Type: "Unidirectional", Context: "authored-by"}, {Source: n.ID, Target: mustResourceID(m.ChannelID), Type: "Unidirectional", Context: "in-channel"}} {
		body, _ := json.Marshal(edge.Target)
		if _, err := g.request(ctx, http.MethodPost, nodePath(n.ID)+"/links", url.Values{"linkType": {edge.Type}, "context": {edge.Context}}, body); err != nil {
			return n.ID, errOutcomeUnknown
		}
	}
	confirmed, err := g.readEntity(ctx, n.ID, "message", owner)
	if err != nil {
		return n.ID, errOutcomeUnknown
	}
	if err := g.validateRelations(ctx, confirmed, m, owner); err != nil {
		return n.ID, err
	}
	if err := g.replaceContent(ctx, n.ID, m); err != nil {
		return n.ID, errOutcomeUnknown
	}
	read, stored, found, err := g.outboundLookup(ctx, name, owner)
	if err != nil || !found || read.ID != n.ID || !reflect.DeepEqual(stored, m) {
		return n.ID, errOutcomeUnknown
	}
	return n.ID, nil
}

func selectOutbound(cfg *authConfig, selected int64, caller principal, broadcaster string) (twitchProfile, bool) {
	var chosen twitchProfile
	count := 0
	for _, candidate := range cfg.twitch {
		if candidate.BroadcasterUserID != broadcaster || !caller.admin() && candidate.TenantUserID != caller.UserID {
			continue
		}
		if p, ok := eligibleTwitch(cfg, selected, candidate.id); ok {
			chosen = p
			count++
		}
	}
	return chosen, count == 1
}

func sameOutboundRequest(m messageContent, input messageInput) bool {
	return m.Service == input.Service && m.Channel == input.Channel && m.User != nil && *m.User == input.User && m.TTS != nil && *m.TTS == input.TTS && reflect.DeepEqual(m.Chunks, input.Chunks) && reflect.DeepEqual(m.ReplyTo, input.ReplyTo)
}

func deliveryLocation(id int64) string {
	return "/api/v3/messages/" + strconv.FormatInt(id, 10)
}
