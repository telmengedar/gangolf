package main

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
)

func (s *server) videoEvent(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "method_not_allowed")
		return
	}
	if r.URL.RawQuery != "" {
		failure(w, 400, "invalid_filter")
		return
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		failure(w, 400, "invalid_body")
		return
	}
	body, ok := resourceBody(w, r)
	if !ok {
		return
	}
	var input struct {
		MessageID string `json:"messageId"`
		TriggerID string `json:"triggerId"`
	}
	var fields map[string]json.RawMessage
	if mediaJSON(body, &fields) != nil || len(fields) != 2 || fields["messageId"] == nil || fields["triggerId"] == nil || strictJSON(body, &input) != nil {
		failure(w, 400, "invalid_body")
		return
	}
	id, validMessage := resourceID(input.MessageID)
	_, validTrigger := resourceID(input.TriggerID)
	if !validMessage || !validTrigger {
		failure(w, 400, "invalid_body")
		return
	}
	if err := lockResources(ctx); err != nil {
		storageFailure(w, err)
		return
	}
	defer resourceLock.Unlock()
	p, cfg, status, code := s.eventAuthorization(ctx, r, false)
	if status != 0 {
		failure(w, status, code)
		return
	}
	m := s.media
	if m == nil {
		failure(w, 503, "media_unavailable")
		return
	}
	if !p.admin() && p.UserID != m.tenant {
		failure(w, 403, "forbidden")
		return
	}
	if err := lockMutex(ctx, &m.mu); err != nil {
		failure(w, 503, "media_unavailable")
		return
	}
	defer m.mu.Unlock()
	catalog, err := m.load(ctx)
	if err != nil {
		failure(w, 503, "media_unavailable")
		return
	}
	all, err := s.graph.messagesV3(ctx, cfg.serviceOwner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	source, found := all[id]
	if !found || source.Deleted {
		failure(w, 404, "not_found")
		return
	}
	policy := s.policy(p, cfg)
	if !policy.visible(source.Service, source.Channel, source.ChannelID) {
		failure(w, 403, "forbidden")
		return
	}
	if !p.admin() && !strings.EqualFold(source.Service, "Twitch") {
		failure(w, 403, "forbidden")
		return
	}
	for _, trigger := range catalog.Triggers {
		if trigger.ID != input.TriggerID {
			continue
		}
		if trigger.ClipID != nil {
			for _, clip := range catalog.Clips {
				if clip.ID != *trigger.ClipID {
					continue
				}
				e := s.publishEvent("video.triggered", source.Service, source.Channel, &source.ChannelID, struct {
					MessageID string       `json:"messageId"`
					Trigger   mediaTrigger `json:"trigger"`
					Clip      mediaClip    `json:"clip"`
				}{input.MessageID, trigger, clip})
				respond(w, 200, e)
				return
			}
		}
		failure(w, 409, "unresolved_trigger")
		return
	}
	failure(w, 404, "not_found")
}
