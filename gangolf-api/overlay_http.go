package main

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

//go:embed overlays/*.html overlays/*.js overlays/*.css
var overlayAssets embed.FS

const overlayPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-src 'self'; base-uri 'none'; form-action 'none'; object-src 'none'"

func (s *server) overlayHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		failure(w, 405, "method_not_allowed")
		return
	}
	if r.ContentLength != 0 {
		failure(w, 400, "invalid_body")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/overlays/")
	name, kind := "", ""
	switch path {
	case "chat", "video":
		filter, ok := overlayFilter(r.URL)
		if !ok {
			failure(w, 400, "invalid_filter")
			return
		}
		request, ok := prepareCredential(r)
		if !ok || r.Header.Get("Authorization") != "" {
			failure(w, 401, "unauthorized")
			return
		}
		authURL := *request.URL
		authURL.Path = "/api/v3/events"
		request = request.Clone(request.Context())
		request.URL = &authURL
		ctx, cancel := context.WithTimeout(r.Context(), eventReadTimeout)
		defer cancel()
		p, _, status, code := s.freshEventAuthorization(ctx, request, false, filter)
		if status != 0 {
			failure(w, status, code)
			return
		}
		if p.Scope != "channel-read" {
			failure(w, 403, "forbidden")
			return
		}
		if path == "video" && s.player == nil {
			failure(w, 503, "player_unavailable")
			return
		}
		name, kind = path+".html", "text/html; charset=utf-8"
	case "shared.js", "chat.js", "video.js":
		name, kind = path, "text/javascript; charset=utf-8"
	case "overlay.css":
		name, kind = path, "text/css; charset=utf-8"
	default:
		failure(w, 404, "not_found")
		return
	}
	if path != "chat" && path != "video" && (r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "") {
		failure(w, 400, "invalid_filter")
		return
	}
	policy := overlayPolicy
	if path == "video" {
		policy = strings.Replace(policy, "frame-src 'self'", "frame-src "+s.player.PlayerOrigin, 1)
	}
	serveOverlayAsset(w, name, kind, policy, s.player)
}

func serveOverlayAsset(w http.ResponseWriter, name, kind, policy string, origins *overlayPlayer) {
	body, err := overlayAssets.ReadFile("overlays/" + name)
	if err != nil {
		failure(w, 404, "not_found")
		return
	}
	if strings.HasSuffix(name, ".html") && origins != nil {
		page, err := template.New("overlay").Parse(string(body))
		if err != nil {
			failure(w, 503, "player_unavailable")
			return
		}
		var rendered bytes.Buffer
		if page.Execute(&rendered, origins) != nil {
			failure(w, 503, "player_unavailable")
			return
		}
		body = rendered.Bytes()
	}
	w.Header().Set("Content-Security-Policy", policy)
	w.Header().Set("Content-Type", kind)
	w.WriteHeader(200)
	w.Write(body)
}

func overlayFilter(u *url.URL) (eventFilter, bool) {
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(u.RawQuery) > 8192 || u.Fragment != "" || !q.Has("apiKey") {
		return eventFilter{}, false
	}
	for key := range q {
		if key != "apiKey" && key != "service" && key != "channel" {
			return eventFilter{}, false
		}
	}
	q.Set("types", "message.created")
	return parseEventFilter(q.Encode())
}
