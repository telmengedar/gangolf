package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func appBody(w http.ResponseWriter, r *http.Request, target any) bool {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		failure(w, 400, "invalid_body")
		return false
	}
	body, ok := resourceBody(w, r)
	if !ok {
		return false
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' || mediaJSON(body, target) != nil {
		failure(w, 400, "invalid_body")
		return false
	}
	return true
}

func appList(w http.ResponseWriter, q url.Values, rows []map[string]any, filters ...string) {
	count, offset := 100, 0
	descending := false
	allowed := map[string]bool{"count": true, "continue": true, "sort": true, "descending": true, "apiKey": true}
	for _, key := range filters {
		allowed[key] = true
	}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 || values[0] == "" {
			failure(w, 400, "invalid_filter")
			return
		}
	}
	var err error
	if q.Has("count") {
		count, err = strconv.Atoi(q.Get("count"))
		if err != nil || count < 1 || count > 500 {
			failure(w, 400, "invalid_filter")
			return
		}
	}
	if q.Has("continue") {
		offset, err = strconv.Atoi(q.Get("continue"))
		if err != nil || offset < 0 {
			failure(w, 400, "invalid_filter")
			return
		}
	}
	if q.Has("sort") && q.Get("sort") != "id" {
		failure(w, 400, "invalid_filter")
		return
	}
	if q.Has("descending") {
		if q.Get("descending") != "true" && q.Get("descending") != "false" {
			failure(w, 400, "invalid_filter")
			return
		}
		descending = q.Get("descending") == "true"
	}
	for _, key := range []string{"enabled"} {
		if q.Has(key) && q.Get(key) != "true" && q.Get(key) != "false" {
			failure(w, 400, "invalid_filter")
			return
		}
	}
	selected := []map[string]any{}
	for _, row := range rows {
		match := true
		for _, key := range filters {
			if q.Has(key) && (key == "enabled" && strconv.FormatBool(row[key].(bool)) != q.Get(key) || key != "enabled" && row[key] != q.Get(key)) {
				match = false
			}
		}
		if match {
			selected = append(selected, row)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i]["id"].(string), selected[j]["id"].(string)
		if descending {
			a, b = b, a
		}
		if selected[i]["scope"] != nil {
			return a < b
		}
		an, ae := strconv.ParseInt(a, 10, 64)
		bn, be := strconv.ParseInt(b, 10, 64)
		if ae == nil && be == nil {
			return an < bn
		}
		return a < b
	})
	start := min(offset, len(selected))
	end := start + min(count, len(selected)-start)
	var next *int
	if end < len(selected) {
		next = &end
	}
	respond(w, 200, map[string]any{"result": selected[start:end], "total": len(selected), "continue": next})
}

func (s *server) appHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.ContentLength != 0 {
		failure(w, 400, "invalid_body")
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
	token, query := credential(r)
	if token == "" {
		failure(w, 401, "unauthorized")
		return
	}
	p, status := s.authorize(ctx, r, cfg, token, query)
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
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v3/"), "/")
	for _, part := range parts {
		if part == "" {
			failure(w, 404, "not_found")
			return
		}
	}
	switch parts[0] {
	case "users":
		id := ""
		if len(parts) > 1 {
			id = parts[1]
			if id == "current" {
				id = p.UserID
			}
		}
		if len(parts) == 3 && parts[2] == "tokens" {
			s.tokensHTTP(ctx, w, r, cfg, p, id, "")
			return
		}
		if len(parts) > 2 {
			failure(w, 404, "not_found")
			return
		}
		s.usersHTTP(ctx, w, r, cfg, p, id)
	case "tokens":
		if len(parts) != 2 {
			failure(w, 404, "not_found")
			return
		}
		s.tokensHTTP(ctx, w, r, cfg, p, "", parts[1])
	case "channels":
		id := ""
		if len(parts) == 2 {
			id = parts[1]
		}
		if len(parts) > 2 {
			failure(w, 404, "not_found")
			return
		}
		s.channelsHTTP(ctx, w, r, cfg, p, id)
	}
}

func allowMethod(w http.ResponseWriter, r *http.Request, allow string) bool {
	for _, method := range strings.Split(allow, ", ") {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", allow)
	failure(w, 405, "method_not_allowed")
	return false
}

func (s *server) userResource(ctx context.Context, cfg *authConfig, id string) (map[string]any, error) {
	user, ok := cfg.Users[id]
	if !ok {
		return nil, nil
	}
	var name *string
	if user.ProfileNodeID != "" {
		nodeID, _ := resourceID(user.ProfileNodeID)
		_, profile, err := s.graph.appProfile(ctx, nodeID, cfg.serviceOwner)
		if err != nil || profile.UserID != id {
			return nil, errCorrupt
		}
		if profile.Deleted {
			return nil, nil
		}
		name = profile.Name
	}
	return map[string]any{"id": id, "name": name, "enabled": user.Enabled, "permissions": user.Permissions}, nil
}

func validPermissions(values []string) bool {
	if len(values) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value != "host" && value != "admin" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func lastAdmin(cfg *authConfig, id string, next grant) bool {
	old := cfg.Users[id]
	if !old.Enabled || !permissionsContain(old.Permissions, "admin") || next.Enabled && permissionsContain(next.Permissions, "admin") {
		return false
	}
	for other, user := range cfg.Users {
		if other != id && user.Enabled && permissionsContain(user.Permissions, "admin") {
			return false
		}
	}
	return true
}

func (s *server) usersHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg *authConfig, p principal, id string) {
	allow := "GET, POST"
	if id != "" {
		allow = "GET, PUT, DELETE"
	}
	if !allowMethod(w, r, allow) {
		return
	}
	q, _ := url.ParseQuery(r.URL.RawQuery)
	if id != "" {
		if _, ok := resourceID(id); !ok {
			failure(w, 400, "invalid_id")
			return
		}
	}
	if !p.admin() && (id == "" || id != p.UserID || r.Method == "DELETE") {
		failure(w, 403, "forbidden")
		return
	}
	if r.Method != "GET" && len(q) > 0 || id != "" && len(q) > 0 {
		failure(w, 400, "invalid_filter")
		return
	}
	if r.Method == "GET" && id == "" {
		rows := []map[string]any{}
		for userID := range cfg.Users {
			row, err := s.userResource(ctx, cfg, userID)
			if err != nil {
				storageFailure(w, err)
				return
			}
			if row != nil {
				rows = append(rows, row)
			}
		}
		appList(w, q, rows, "enabled")
		return
	}
	if r.Method == "POST" {
		var input struct {
			Name        *string  `json:"name"`
			Enabled     *bool    `json:"enabled"`
			Permissions []string `json:"permissions"`
		}
		if !appBody(w, r, &input) {
			return
		}
		if !validAppName(input.Name, true, 256) || input.Enabled == nil || !validPermissions(input.Permissions) {
			failure(w, 400, "invalid_user")
			return
		}
		for _, permission := range input.Permissions {
			if !permissionsContain(effectivePermissions([]string{permission}, p.Permissions), permission) {
				failure(w, 403, "forbidden")
				return
			}
		}
		n, err := s.graph.createAppProfile(ctx, cfg.serviceOwner, "", input.Name)
		if err != nil {
			storageFailure(w, err)
			return
		}
		id = strconv.FormatInt(n, 10)
		if _, exists := cfg.Users[id]; exists {
			failure(w, 409, "user_exists")
			return
		}
		cfg.Users[id] = grant{Enabled: *input.Enabled, Permissions: input.Permissions, ProfileNodeID: id, ManagedOnly: true}
		if err = s.graph.saveAuth(ctx, cfg); err != nil {
			storageFailure(w, err)
			return
		}
		row, err := s.userResource(ctx, cfg, id)
		if err != nil {
			storageFailure(w, err)
			return
		}
		w.Header().Set("Location", "/api/v3/users/"+id)
		respond(w, 201, row)
		return
	}
	row, err := s.userResource(ctx, cfg, id)
	if err != nil {
		storageFailure(w, err)
		return
	}
	if row == nil {
		failure(w, 404, "not_found")
		return
	}
	if r.Method == "GET" {
		respond(w, 200, row)
		return
	}
	user := cfg.Users[id]
	if r.Method == "DELETE" {
		next := user
		next.Enabled = false
		if lastAdmin(cfg, id, next) {
			failure(w, 409, "last_admin")
			return
		}
		n, err := s.ensureProfile(ctx, cfg, id, nil)
		if err != nil {
			storageFailure(w, err)
			return
		}
		next = cfg.Users[id]
		next.Enabled = false
		cfg.Users[id] = next
		for key, value := range cfg.APIKeys {
			if value.UserID == id {
				delete(cfg.APIKeys, key)
			}
		}
		if err = s.graph.saveAuth(ctx, cfg); err != nil {
			storageFailure(w, err)
			return
		}
		_, profile, err := s.graph.appProfile(ctx, n, cfg.serviceOwner)
		if err != nil {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		profile.Deleted = true
		_ = s.graph.replaceContent(ctx, n, profile)
		_, actual, err := s.graph.appProfile(ctx, n, cfg.serviceOwner)
		if err != nil || !sameJSON(profile, actual) {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		w.WriteHeader(204)
		return
	}
	var input struct {
		ID          string   `json:"id"`
		Name        *string  `json:"name"`
		Enabled     *bool    `json:"enabled"`
		Permissions []string `json:"permissions"`
	}
	if !appBody(w, r, &input) {
		return
	}
	if input.ID != "" && input.ID != id || !validAppName(input.Name, true, 256) || input.Enabled == nil || !validPermissions(input.Permissions) {
		failure(w, 400, "invalid_user")
		return
	}
	if !p.admin() && (*input.Enabled != user.Enabled || !sameJSON(input.Permissions, user.Permissions)) {
		failure(w, 403, "forbidden")
		return
	}
	next := user
	next.Enabled = *input.Enabled
	next.Permissions = input.Permissions
	if lastAdmin(cfg, id, next) {
		failure(w, 409, "last_admin")
		return
	}
	n, err := s.ensureProfile(ctx, cfg, id, input.Name)
	if err != nil {
		storageFailure(w, err)
		return
	}
	next.ProfileNodeID = cfg.Users[id].ProfileNodeID
	cfg.Users[id] = next
	if err = s.graph.saveAuth(ctx, cfg); err != nil {
		storageFailure(w, err)
		return
	}
	_, profile, err := s.graph.appProfile(ctx, n, cfg.serviceOwner)
	if err != nil {
		storageFailure(w, errOutcomeUnknown)
		return
	}
	profile.Name = input.Name
	_ = s.graph.replaceContent(ctx, n, profile)
	_, actual, err := s.graph.appProfile(ctx, n, cfg.serviceOwner)
	if err != nil || !sameJSON(actual, profile) {
		storageFailure(w, errOutcomeUnknown)
		return
	}
	row, err = s.userResource(ctx, cfg, id)
	if err != nil {
		storageFailure(w, err)
		return
	}
	respond(w, 200, row)
}

func tokenResource(id string, k keyGrant, now time.Time) map[string]any {
	scope := k.Scope
	if scope == "" {
		scope = "api"
	}
	ids := k.ChannelIDs
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{"id": id, "userId": k.UserID, "name": k.Name, "scope": scope, "channelIds": ids, "enabled": k.Enabled && (k.ExpiresAt == nil || k.ExpiresAt.After(now)), "permissions": k.Permissions, "expiresAt": k.ExpiresAt}
}

func (s *server) issueToken(ctx context.Context, cfg *authConfig, userID string, name *string, scope string, channels, permissions []string, expires *time.Time) (string, string, error) {
	idBytes := make([]byte, 16)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", errUnavailable
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", errUnavailable
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	if _, exists := cfg.APIKeys[id]; exists {
		return "", "", errUnavailable
	}
	token := id + "." + base64.RawURLEncoding.EncodeToString(secretBytes)
	hash := hmac.New(sha256.New, []byte(s.pepper))
	hash.Write([]byte(token))
	cfg.APIKeys[id] = keyGrant{UserID: userID, Hash: hash.Sum(nil), Enabled: true, Permissions: permissions, ExpiresAt: expires, Name: name, Scope: scope, ChannelIDs: channels}
	if err := s.graph.saveAuth(ctx, cfg); err != nil {
		return "", "", err
	}
	return id, token, nil
}

func (s *server) tokensHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg *authConfig, p principal, userID, id string) {
	allow := "GET, POST"
	if id != "" {
		allow = "GET, DELETE"
	}
	if !allowMethod(w, r, allow) {
		return
	}
	if id != "" {
		key, ok := cfg.APIKeys[id]
		if !ok {
			failure(w, 404, "not_found")
			return
		}
		userID = key.UserID
	}
	user, ok := cfg.Users[userID]
	if !ok {
		failure(w, 404, "not_found")
		return
	}
	if !p.admin() && p.UserID != userID {
		failure(w, 403, "forbidden")
		return
	}
	q, _ := url.ParseQuery(r.URL.RawQuery)
	if r.Method != "GET" && len(q) > 0 || id != "" && len(q) > 0 {
		failure(w, 400, "invalid_filter")
		return
	}
	if r.Method == "DELETE" {
		delete(cfg.APIKeys, id)
		if err := s.graph.saveAuth(ctx, cfg); err != nil {
			storageFailure(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method == "GET" {
		if id != "" {
			respond(w, 200, tokenResource(id, cfg.APIKeys[id], s.now()))
			return
		}
		rows := []map[string]any{}
		for key, value := range cfg.APIKeys {
			if value.UserID == userID {
				rows = append(rows, tokenResource(key, value, s.now()))
			}
		}
		appList(w, q, rows, "enabled")
		return
	}
	if !user.Enabled {
		failure(w, 403, "forbidden")
		return
	}
	var input struct {
		Name        *string     `json:"name"`
		Scope       string      `json:"scope"`
		ChannelIDs  []string    `json:"channelIds"`
		Permissions []string    `json:"permissions"`
		ExpiresAt   tokenExpiry `json:"expiresAt"`
	}
	if !appBody(w, r, &input) {
		return
	}
	if input.Name != nil && (!utf8.ValidString(*input.Name) || utf8.RuneCountInString(*input.Name) > 128) {
		failure(w, 400, "invalid_token")
		return
	}
	if input.Scope == "" {
		input.Scope = "api"
	}
	maximum, def := 365*24*time.Hour, 30*24*time.Hour
	if input.Permissions == nil {
		input.Permissions = []string{"host"}
	}
	if !validPermissions(input.Permissions) {
		failure(w, 400, "invalid_token")
		return
	}
	if input.Scope == "channel-read" {
		maximum, def = 30*24*time.Hour, 24*time.Hour
		if len(input.ChannelIDs) == 0 || !sameJSON(input.Permissions, []string{"host"}) {
			failure(w, 400, "invalid_token")
			return
		}
		policy := s.policy(principal{UserID: userID, Permissions: []string{"host"}, ManagedOnly: user.ManagedOnly}, cfg)
		seen := map[string]bool{}
		for _, channelID := range input.ChannelIDs {
			if seen[channelID] {
				failure(w, 400, "invalid_token")
				return
			}
			seen[channelID] = true
			allowed, err := policy.scopedReadChannel(ctx, channelID, true)
			if err != nil {
				storageFailure(w, err)
				return
			}
			if !allowed {
				failure(w, 403, "forbidden")
				return
			}
		}
	} else if input.Scope != "api" || len(input.ChannelIDs) != 0 {
		failure(w, 400, "invalid_token")
		return
	}
	for _, permission := range input.Permissions {
		if !permissionsContain(effectivePermissions([]string{permission}, user.Permissions), permission) || !permissionsContain(effectivePermissions([]string{permission}, p.Permissions), permission) {
			failure(w, 403, "forbidden")
			return
		}
	}
	now := s.now().UTC()
	expires := input.ExpiresAt.value
	if !input.ExpiresAt.present || expires != nil {
		deadline := now.Add(maximum)
		if p.ExpiresAt != nil && p.ExpiresAt.Before(deadline) {
			deadline = *p.ExpiresAt
		}
		if expires == nil {
			value := now.Add(def)
			if value.After(deadline) {
				value = deadline
			}
			expires = &value
		}
		if !expires.After(now) || expires.After(deadline) {
			failure(w, 400, "invalid_token")
			return
		}
		utcExpiry := expires.UTC()
		expires = &utcExpiry
	}
	id, token, err := s.issueToken(ctx, cfg, userID, input.Name, input.Scope, input.ChannelIDs, input.Permissions, expires)
	if err != nil {
		storageFailure(w, err)
		return
	}
	row := tokenResource(id, cfg.APIKeys[id], now)
	row["token"] = token
	w.Header().Set("Location", "/api/v3/tokens/"+id)
	respond(w, 201, row)
}
