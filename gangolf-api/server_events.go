package main

import (
	"context"
	"log/slog"
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
	if err != nil || len(raw) > 8192 {
		return f, false
	}
	for k, values := range q {
		if len(values) != 1 || values[0] == "" || !utf8.ValidString(values[0]) {
			return f, false
		}
		switch k {
		case "apiKey":
			continue
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

func (s *server) eventAuthorization(ctx context.Context, r *http.Request) (principal, *authConfig, int, string) {
	ctx, cancel := context.WithTimeout(ctx, eventReadTimeout)
	defer cancel()
	token, query := credential(r)
	if token == "" {
		return principal{}, nil, 401, "unauthorized"
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return principal{}, nil, 503, "storage_unavailable"
	}
	p, status := s.authorize(ctx, r, cfg, token, query)
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
	p, _, status, code := s.freshEventAuthorization(ctx, r, filter.types["video.triggered"], filter, nil)
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
	slog.Debug("event stream ready", "replay", false)
	reason := "disconnected"
	defer func() { slog.Debug("event stream closed", "reason", reason) }()
	ticker := time.NewTicker(eventHeartbeat)
	defer ticker.Stop()
	for {
		var frame eventFrame
		var data bool
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			reason = "subscriber_closed"
			return
		case frame = <-sub.queue:
			if !h.consume(sub, frame) {
				return
			}
			data = true
		case <-ticker.C:
		}
		var source *eventEnvelope
		if data {
			source = &frame.envelope
		}
		p, allowed, status, code := s.freshEventAuthorization(ctx, r, filter.types["video.triggered"], filter, source)
		if status != 0 || p.UserID != sub.user || !h.active(sub) {
			reason = "authorization_failed"
			if status != 0 {
				reason = code
			}
			return
		}
		if data {
			if !allowed {
				slog.Debug("event frame suppressed", "reason", "source_unauthorized")
				continue
			}

			if !writeEventFrame(w, control, frame.bytes) {
				reason = "write_failed"
				return
			}
		} else if !writeEventFrame(w, control, []byte(": heartbeat\n\n")) {
			reason = "write_failed"
			return
		}
	}
}

func (s *server) freshEventAuthorization(ctx context.Context, r *http.Request, video bool, filter eventFilter, source *eventEnvelope) (principal, bool, int, string) {
	ctx, cancel := context.WithTimeout(ctx, eventReadTimeout)
	defer cancel()
	initialSource := source
	if video {
		initialSource = nil
	}
	p, allowed, status, code := s.primaryEventAuthorization(ctx, r, filter, initialSource)
	if status != 0 || !video {
		return p, allowed, status, code
	}
	media := s.media
	if media == nil || lockMutex(ctx, &media.mu) != nil {
		return p, false, 503, "media_unavailable"
	}
	_, err := media.load(ctx)
	media.mu.Unlock()
	if err != nil {
		return p, false, 503, "media_unavailable"
	}
	return s.primaryEventAuthorization(ctx, r, filter, source)
}

func (s *server) primaryEventAuthorization(ctx context.Context, r *http.Request, filter eventFilter, source *eventEnvelope) (principal, bool, int, string) {
	if err := lockResources(ctx); err != nil {
		return principal{}, false, 503, "storage_unavailable"
	}
	defer resourceLock.Unlock()
	p, cfg, status, code := s.eventAuthorization(ctx, r)
	if status != 0 {
		return p, false, status, code
	}
	policy := s.policy(p, cfg)
	if filter.channel != "" {
		allowed, err := policy.allowed(ctx, filter.service, filter.channel, "", true)
		if err != nil {
			return p, false, 503, "storage_unavailable"
		}
		if !allowed {
			return p, false, 403, "forbidden"
		}
	}
	if p.Scope == "channel-read" {
		for _, id := range p.ChannelIDs {
			allowed, err := policy.allowed(ctx, "", "", id, true)
			if err != nil {
				return p, false, 503, "storage_unavailable"
			}
			if !allowed {
				return p, false, 403, "forbidden"
			}
		}
	}
	if source != nil {
		channelID := ""
		if source.ChannelID != nil {
			channelID = *source.ChannelID
		}
		allowed, err := policy.allowed(ctx, source.Service, source.Channel, channelID, true)
		if err != nil {
			return p, false, 503, "storage_unavailable"
		}
		return p, allowed, 0, ""
	}
	return p, true, 0, ""
}
