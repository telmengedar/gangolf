package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func channelResource(n node, v identityContent, owner string) map[string]any {
	var ownerID *string
	if owner != "" {
		ownerID = &owner
	}
	enabled := true
	if v.Enabled != nil {
		enabled = *v.Enabled
	}
	return map[string]any{"id": strconv.FormatInt(n.ID, 10), "service": v.Service, "channel": v.Channel, "ownerUserId": ownerID, "enabled": enabled, "deleted": v.Deleted}
}

func (s *server) channelsHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg *authConfig, p principal, id string) {
	allow := "GET, POST"
	if id != "" {
		allow = "GET, PUT, DELETE"
	}
	if !allowMethod(w, r, allow) {
		return
	}
	q, _ := url.ParseQuery(r.URL.RawQuery)
	if r.Method != "GET" && len(q) > 0 || id != "" && len(q) > 0 && !(len(q) == 1 && q.Has("apiKey")) {
		failure(w, 400, "invalid_filter")
		return
	}
	policy := s.policy(p, cfg)
	if r.Method == "POST" {
		var input struct {
			Service     string  `json:"service"`
			Channel     string  `json:"channel"`
			OwnerUserID *string `json:"ownerUserId"`
			Enabled     *bool   `json:"enabled"`
		}
		if !appBody(w, r, &input) {
			return
		}
		if strings.TrimSpace(input.Service) == "" || strings.TrimSpace(input.Channel) == "" || !validAppName(&input.Service, true, 256) || !validAppName(&input.Channel, true, 256) {
			failure(w, 400, "invalid_channel")
			return
		}
		owner := p.UserID
		if input.OwnerUserID != nil {
			if !p.admin() && *input.OwnerUserID != owner {
				failure(w, 403, "forbidden")
				return
			}
			owner = *input.OwnerUserID
		}
		user, ok := cfg.Users[owner]
		if !ok || !user.Enabled {
			failure(w, 403, "forbidden")
			return
		}
		name := identityName("channel", input.Service, input.Channel)
		existing, err := s.graph.identityIntegrity(ctx, "channel", name, cfg.serviceOwner)
		if err != nil {
			storageFailure(w, err)
			return
		}
		if len(existing) > 0 {
			failure(w, 409, "channel_exists")
			return
		}
		root, err := s.ensureProfile(ctx, cfg, owner, nil)
		if err != nil {
			storageFailure(w, err)
			return
		}
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		value := identityContent{SchemaVersion: 2, Service: input.Service, Channel: input.Channel, Enabled: &enabled}
		body, _ := json.Marshal(map[string]any{"name": name, "type": "channel", "rootNodeId": root, "access": 0, "skipEmbedding": true})
		data, err := s.graph.request(ctx, http.MethodPost, "/nodes", nil, body)
		if err != nil {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		var n node
		if json.Unmarshal(data, &n) != nil || n.Root != root || !s.graph.validChannelNode(ctx, n, cfg.serviceOwner) || n.Type != "channel" || n.Name != name {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		_ = s.graph.addPlainLink(ctx, n.ID, s.graph.groupsChannel())
		_ = s.graph.replaceContent(ctx, n.ID, value)
		confirmed, err := s.graph.readEntity(ctx, n.ID, "channel", cfg.serviceOwner)
		if err != nil {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		actual, err := channelIdentity(confirmed)
		unique, uniqueErr := s.graph.identityIntegrity(ctx, "channel", name, cfg.serviceOwner)
		if err != nil || uniqueErr != nil || !sameJSON(actual, value) || len(unique) != 1 || unique[0].ID != n.ID {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		w.Header().Set("Location", "/api/v3/channels/"+strconv.FormatInt(n.ID, 10))
		respond(w, 201, channelResource(confirmed, value, owner))
		return
	}
	if id == "" {
		nodes, err := s.graph.resourceNodes(ctx, "channel", "", cfg.serviceOwner)
		if err != nil {
			storageFailure(w, err)
			return
		}
		rows := []map[string]any{}
		for _, n := range nodes {
			value, err := channelIdentity(n)
			if err != nil {
				storageFailure(w, err)
				return
			}
			visible, err := policy.allowed(ctx, value.Service, value.Channel, strconv.FormatInt(n.ID, 10), false)
			if err != nil {
				storageFailure(w, err)
				return
			}
			if visible && !value.Deleted {
				owner, _ := policy.channelOwner(n)
				rows = append(rows, channelResource(n, value, owner))
			}
		}
		appList(w, q, rows, "service", "channel", "enabled")
		return
	}
	parsed, ok := resourceID(id)
	if !ok {
		failure(w, 400, "invalid_id")
		return
	}
	rows, err := s.graph.queryResourceItem(ctx, "channel", "", cfg.serviceOwner, s.graph.groupsChannel(), parsed)
	if err != nil {
		storageFailure(w, err)
		return
	}
	if len(rows) == 0 {
		failure(w, 404, "not_found")
		return
	}
	if len(rows) != 1 {
		storageFailure(w, errCorrupt)
		return
	}
	n := rows[0]
	value, err := channelIdentity(n)
	if err != nil {
		storageFailure(w, err)
		return
	}
	visible, err := policy.allowed(ctx, value.Service, value.Channel, id, false)
	if err != nil {
		storageFailure(w, err)
		return
	}
	if !visible || value.Deleted {
		failure(w, 404, "not_found")
		return
	}
	owner, valid := policy.channelOwner(n)
	if !valid {
		storageFailure(w, errCorrupt)
		return
	}
	if r.Method == "GET" {
		respond(w, 200, channelResource(n, value, owner))
		return
	}
	if owner == "" {
		failure(w, 403, "forbidden")
		return
	}
	if r.Method == "DELETE" {
		disabled := false
		value.Enabled = &disabled
		value.Deleted = true
	} else {
		var input struct {
			ID          string  `json:"id"`
			Service     string  `json:"service"`
			Channel     string  `json:"channel"`
			OwnerUserID *string `json:"ownerUserId"`
			Enabled     *bool   `json:"enabled"`
			Deleted     bool    `json:"deleted"`
		}
		if !appBody(w, r, &input) {
			return
		}
		if input.ID != "" && input.ID != id || input.Service != value.Service || input.Channel != value.Channel || input.OwnerUserID != nil && *input.OwnerUserID != owner || input.Enabled == nil || input.Deleted {
			failure(w, 400, "invalid_channel")
			return
		}
		value.Enabled = input.Enabled
	}
	_ = s.graph.replaceContent(ctx, n.ID, value)
	confirmed, err := s.graph.readEntity(ctx, n.ID, "channel", cfg.serviceOwner)
	if err != nil {
		storageFailure(w, errOutcomeUnknown)
		return
	}
	actual, err := channelIdentity(confirmed)
	if err != nil || !sameJSON(actual, value) {
		storageFailure(w, errOutcomeUnknown)
		return
	}
	if r.Method == "DELETE" {
		w.WriteHeader(204)
		return
	}
	respond(w, 200, channelResource(confirmed, value, owner))
}
