package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

type appUser struct {
	SchemaVersion int     `json:"schemaVersion"`
	UserID        string  `json:"userId"`
	Name          *string `json:"name"`
	Deleted       bool    `json:"deleted"`
}

func validAppName(name *string, required bool, limit int) bool {
	return name == nil && !required || name != nil && utf8.ValidString(*name) && strings.TrimSpace(*name) != "" && utf8.RuneCountInString(*name) <= limit
}

func (g *graph) appProfile(ctx context.Context, id int64, owner int64) (node, appUser, error) {
	q := url.Values{"id": {strconv.FormatInt(id, 10)}, "rootNodeId": {strconv.FormatInt(g.root, 10)}, "type": {"gangolf-user"}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails,skipEmbedding"}, "count": {"2"}}
	data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
	var page graphPage
	var profile appUser
	if err != nil {
		return node{}, profile, err
	}
	if json.Unmarshal(data, &page) != nil || page.Total != 1 || len(page.Result) != 1 || page.Continue != nil {
		return node{}, profile, errCorrupt
	}
	n := page.Result[0]
	if n.ID != id || n.Type != "gangolf-user" || !g.privateNode(n, owner) || n.SkipEmbedding == nil || !*n.SkipEmbedding || len(n.Links) != 0 || n.Content == nil || !strings.HasPrefix(n.ContentType, "application/json") || mediaJSON([]byte(*n.Content), &profile) != nil || profile.SchemaVersion != 1 || !positiveDecimal(profile.UserID) || !validAppName(profile.Name, false, 256) {
		return node{}, profile, errCorrupt
	}
	return n, profile, nil
}

func (g *graph) createAppProfile(ctx context.Context, owner int64, userID string, name *string) (int64, error) {
	body, _ := json.Marshal(map[string]any{"name": "Gangolf app user", "type": "gangolf-user", "rootNodeId": g.root, "access": 0, "skipEmbedding": true})
	data, err := g.request(ctx, http.MethodPost, "/nodes", nil, body)
	if err != nil {
		return 0, errOutcomeUnknown
	}
	var n node
	if json.Unmarshal(data, &n) != nil || !g.privateNode(n, owner) || n.Type != "gangolf-user" {
		return 0, errOutcomeUnknown
	}
	if userID == "" {
		userID = strconv.FormatInt(n.ID, 10)
	}
	profile := appUser{SchemaVersion: 1, UserID: userID, Name: name}
	_ = g.replaceContent(ctx, n.ID, profile)
	_, actual, err := g.appProfile(ctx, n.ID, owner)
	if err != nil || !reflect.DeepEqual(actual, profile) {
		return 0, errOutcomeUnknown
	}
	return n.ID, nil
}

func (g *graph) saveAuth(ctx context.Context, cfg *authConfig) error {
	_ = g.replaceContent(ctx, g.config, cfg)
	actual, err := g.loadConfig(ctx)
	if err != nil {
		return errOutcomeUnknown
	}
	expectedJSON, _ := json.Marshal(cfg)
	actualJSON, _ := json.Marshal(actual)
	if string(expectedJSON) != string(actualJSON) {
		return errOutcomeUnknown
	}
	return nil
}

func (s *server) ensureProfile(ctx context.Context, cfg *authConfig, id string, name *string) (int64, error) {
	user, ok := cfg.Users[id]
	if !ok {
		return 0, errCorrupt
	}
	if user.ProfileNodeID != "" {
		n, _ := resourceID(user.ProfileNodeID)
		_, profile, err := s.graph.appProfile(ctx, n, cfg.serviceOwner)
		if err != nil || profile.UserID != id || profile.Deleted {
			return 0, errCorrupt
		}
		return n, nil
	}
	n, err := s.graph.createAppProfile(ctx, cfg.serviceOwner, id, name)
	if err != nil {
		return 0, err
	}
	user.ProfileNodeID = strconv.FormatInt(n, 10)
	cfg.Users[id] = user
	if err = s.graph.saveAuth(ctx, cfg); err != nil {
		return 0, err
	}
	return n, nil
}
