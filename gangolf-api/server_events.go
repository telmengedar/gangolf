package main

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type eventFilter struct {
	types   map[string]bool
	service string
	channel string
}

func (f eventFilter) matches(e eventEnvelope) bool {
	return f.types[e.Type] && (f.service == "" || f.service == e.Service) && (f.channel == "" || f.channel == e.Channel)
}

func parseEventFilter(raw string) (eventFilter, bool) {
	f := eventFilter{types: make(map[string]bool)}
	q, err := url.ParseQuery(raw)
	if err != nil || len(raw) > 4096 {
		return f, false
	}
	for k, values := range q {
		if len(values) != 1 || values[0] == "" || !utf8.ValidString(values[0]) {
			return f, false
		}
		switch k {
		case "types":
			for _, kind := range strings.Split(values[0], ",") {
				if kind != "message.created" && kind != "video.triggered" || f.types[kind] {
					return f, false
				}
				f.types[kind] = true
			}
		case "service":
			f.service = values[0]
		case "channel":
			f.channel = values[0]
		default:
			return f, false
		}
	}
	return f, len(f.types) > 0 && (f.channel == "" || f.service != "") && utf8.RuneCountInString(f.service) <= 256 && utf8.RuneCountInString(f.channel) <= 256
}

func acceptsEvents(raw string) bool {
	if raw == "" {
		return true
	}
	best, accepted := -1, false
	for _, item := range strings.Split(raw, ",") {
		kind, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		quality := 1.0
		if q, ok := params["q"]; ok {
			quality, err = strconv.ParseFloat(q, 64)
		}
		specificity := -1
		switch kind {
		case "*/*":
			specificity = 0
		case "text/*":
			specificity = 1
		case "text/event-stream":
			specificity = 2
		}
		if err == nil && specificity > best {
			best = specificity
			accepted = quality > 0 && quality <= 1
		}
	}
	return accepted
}

func (s *server) eventAuthorization(ctx context.Context, r *http.Request, video bool) (principal, *authConfig, int, string) {
	ctx, cancel := context.WithTimeout(ctx, eventReadTimeout)
	defer cancel()
	token, query := credential(r)
	if token == "" || query {
		return principal{}, nil, 401, "unauthorized"
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return principal{}, nil, 503, "storage_unavailable"
	}
	p, status := s.authorize(ctx, r, cfg, token, false)
	if status != 0 {
		code := "unauthorized"
		if status == 403 {
			code = "forbidden"
		}
		if status == 503 {
			code = "auth_unavailable"
		}
		return p, cfg, status, code
	}
	if video {
		m := s.media
		if m == nil {
			return p, cfg, 503, "media_unavailable"
		}
		if !p.admin() && p.UserID != m.tenant {
			return p, cfg, 403, "forbidden"
		}
		if err := lockMutex(ctx, &m.mu); err != nil {
			return p, cfg, 503, "media_unavailable"
		}
		_, err = m.load(ctx)
		m.mu.Unlock()
		if err != nil {
			return p, cfg, 503, "media_unavailable"
		}
	}
	return p, cfg, 0, ""
}

func eventWriterSupported(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(interface{ FlushError() error }); ok {
			return true
		}
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

func writeEventFrame(w http.ResponseWriter, control *http.ResponseController, frame []byte) bool {
	if control.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) != nil {
		return false
	}
	if _, err := w.Write(frame); err != nil {
		return false
	}
	if control.Flush() != nil {
		return false
	}
	return control.SetWriteDeadline(time.Time{}) == nil
}

func (s *server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		failure(w, 405, "method_not_allowed")
		return
	}
	if r.ContentLength != 0 {
		failure(w, 400, "invalid_body")
		return
	}
	if r.Header.Get("Last-Event-ID") != "" {
		failure(w, 409, "replay_not_supported")
		return
	}
	if !acceptsEvents(strings.Join(r.Header.Values("Accept"), ",")) {
		failure(w, 406, "not_acceptable")
		return
	}
	filter, ok := parseEventFilter(r.URL.RawQuery)
	if q, err := url.ParseQuery(r.URL.RawQuery); err == nil && q.Has("apiKey") {
		failure(w, 401, "unauthorized")
		return
	}
	if !ok {
		failure(w, 400, "invalid_filter")
		return
	}
	h := s.eventHub()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sub, status := h.subscribe("", filter, cancel)
	if status != 0 {
		code := "stream_limit"
		if status == 503 {
			code = "stream_unavailable"
		}
		failure(w, status, code)
		return
	}
	defer h.remove(sub)
	p, _, status, code := s.eventAuthorization(ctx, r, filter.types["video.triggered"])
	if status != 0 {
		failure(w, status, code)
		return
	}
	if !h.bind(sub, p.UserID) {
		failure(w, 429, "stream_limit")
		return
	}
	control := http.NewResponseController(w)
	if !eventWriterSupported(w) || control.SetWriteDeadline(time.Time{}) != nil {
		failure(w, 503, "stream_unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if !writeEventFrame(w, control, []byte("event: stream.ready\ndata: {\"schemaVersion\":1,\"replay\":false,\"resyncRequired\":true}\n\n")) {
		return
	}
	ticker := time.NewTicker(eventHeartbeat)
	defer ticker.Stop()
	for {
		var frame eventFrame
		var data bool
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			return
		case frame = <-sub.queue:
			if !h.consume(sub, frame) {
				return
			}
			data = true
		case <-ticker.C:
		}
		p, cfg, status, _ := s.eventAuthorization(ctx, r, filter.types["video.triggered"])
		if status != 0 || p.UserID != sub.user || !h.active(sub) {
			return
		}
		if data {
			channelID := ""
			if frame.envelope.ChannelID != nil {
				channelID = *frame.envelope.ChannelID
			}
			if !s.policy(p, cfg).visible(frame.envelope.Service, frame.envelope.Channel, channelID) {
				continue
			}
			if frame.envelope.Type == "video.triggered" && !p.admin() && !strings.EqualFold(frame.envelope.Service, "Twitch") {
				continue
			}
			if !writeEventFrame(w, control, frame.bytes) {
				return
			}
		} else if !writeEventFrame(w, control, []byte(": heartbeat\n\n")) {
			return
		}
	}
}
