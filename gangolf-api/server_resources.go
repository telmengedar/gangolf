package main

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var resourceLock sync.Mutex

func (s *server) v3(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	item := r.URL.Path != "/api/v3/messages"
	var id int64
	if item {
		raw := strings.TrimPrefix(r.URL.Path, "/api/v3/messages/")
		if raw == "" || strings.Contains(raw, "/") {
			failure(w, 404, "not_found")
			return
		}
		var ok bool
		if id, ok = resourceID(raw); !ok {
			failure(w, 400, "invalid_id")
			return
		}
	}
	allow := "GET, POST"
	validMethod := r.Method == http.MethodGet || r.Method == http.MethodPost
	if item {
		allow = "GET, PATCH, DELETE"
		validMethod = r.Method == http.MethodGet || r.Method == http.MethodPatch || r.Method == http.MethodDelete
	}
	if !validMethod {
		w.Header().Set("Allow", allow)
		failure(w, 405, "method_not_allowed")
		return
	}
	token, queryKey := credential(r)
	if token == "" {
		failure(w, 401, "unauthorized")
		return
	}
	resourceLock.Lock()
	defer resourceLock.Unlock()
	if ctx.Err() != nil {
		storageFailure(w, errUnavailable)
		return
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		storageFailure(w, err)
		return
	}
	if err := s.graph.privateDomainGroups(ctx, cfg.serviceOwner); err != nil {
		storageFailure(w, err)
		return
	}
	if status := s.authorize(ctx, r, cfg, token, queryKey); status != 0 {
		code := "unauthorized"
		if status == 403 {
			code = "forbidden"
		} else if status == 503 {
			code = "auth_unavailable"
		}
		failure(w, status, code)
		return
	}
	if r.Method == http.MethodPost {
		s.createV3(ctx, w, r, cfg.serviceOwner)
		return
	}
	if !item {
		s.listV3(ctx, w, r, cfg.serviceOwner)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) > 0 && (r.Method != http.MethodGet || len(q) != 1 || len(q["apiKey"]) != 1) {
		failure(w, 400, "invalid_filter")
		return
	}
	var operations []patchOperation
	if r.Method == http.MethodPatch {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json-patch+json" {
			failure(w, 400, "invalid_patch")
			return
		}
		body, ok := resourceBody(w, r)
		if !ok {
			return
		}
		if strictJSON(body, &operations) != nil || len(operations) == 0 {
			failure(w, 400, "invalid_patch")
			return
		}
	}
	all, err := s.graph.messagesV3(ctx, cfg.serviceOwner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	m, found := all[id]
	if !found || m.Deleted && r.Method != http.MethodDelete {
		failure(w, 404, "not_found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		respond(w, 200, m.resource(id))
	case http.MethodPatch:
		updated, ok := applyMessagePatch(m, operations)
		if !ok {
			failure(w, 400, "invalid_patch")
			return
		}
		if err := s.graph.replaceContent(ctx, id, updated); err != nil {
			storageFailure(w, errOutcomeUnknown)
			return
		}
		respond(w, 200, updated.resource(id))
	case http.MethodDelete:
		if !m.Deleted {
			tombstone := messageContent{SchemaVersion: 1, Deleted: true, Time: m.Time, UserID: m.UserID, ChannelID: m.ChannelID, ReplyTo: m.ReplyTo}
			if err := s.graph.replaceContent(ctx, id, tombstone); err != nil {
				storageFailure(w, errOutcomeUnknown)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(204)
	}
}

func resourceBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			failure(w, 413, "body_too_large")
		} else {
			failure(w, 400, "invalid_body")
		}
		return nil, false
	}
	return data, true
}

func (s *server) createV3(ctx context.Context, w http.ResponseWriter, r *http.Request, owner int64) {
	if r.URL.RawQuery != "" {
		failure(w, 400, "invalid_filter")
		return
	}
	data, ok := resourceBody(w, r)
	if !ok {
		return
	}
	var input messageInput
	if strictJSON(data, &input) != nil {
		failure(w, 400, "invalid_message")
		return
	}
	if nonNull(input.PostTo) {
		failure(w, 422, "delivery_not_supported")
		return
	}
	if !input.snapshot().valid() {
		failure(w, 400, "invalid_message")
		return
	}
	var parent messageContent
	if input.ReplyTo != nil {
		pid, ok := resourceID(*input.ReplyTo)
		if !ok {
			failure(w, 400, "invalid_message")
			return
		}
		all, err := s.graph.messagesV3(ctx, owner)
		if err != nil {
			storageFailure(w, err)
			return
		}
		var found bool
		parent, found = all[pid]
		if !found || parent.Deleted {
			failure(w, 404, "not_found")
			return
		}
		if parent.Service != input.Service || parent.Channel != input.Channel {
			failure(w, 422, "invalid_reply_target")
			return
		}
	}
	uid, err := s.graph.resolveIdentity(ctx, "user", identityContent{SchemaVersion: 1, Service: input.User.Service, ServiceID: input.User.ServiceID, Name: input.User.Name}, owner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	cid, err := s.graph.resolveIdentity(ctx, "channel", identityContent{SchemaVersion: 1, Service: input.Service, Channel: input.Channel}, owner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	channelID := strconv.FormatInt(cid, 10)
	if input.ReplyTo != nil && parent.ChannelID != channelID {
		storageFailure(w, errCorrupt)
		return
	}
	m := messageContent{SchemaVersion: 1, UserID: strconv.FormatInt(uid, 10), ChannelID: channelID, ReplyTo: input.ReplyTo, Time: s.now().UTC(), Service: input.Service, Channel: input.Channel, User: &input.User, Chunks: input.Chunks, TTS: &input.TTS}
	id, err := s.graph.publishMessage(ctx, m, owner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	w.Header().Set("Location", "/api/v3/messages/"+strconv.FormatInt(id, 10))
	respond(w, 201, m.resource(id))
}

func applyMessagePatch(m messageContent, operations []patchOperation) (messageContent, bool) {
	for _, op := range operations {
		if op.Op != "replace" || !nonNull(op.Value) {
			return m, false
		}
		switch op.Path {
		case "/chunks":
			var chunks []chunk
			if strictJSON(op.Value, &chunks) != nil {
				return m, false
			}
			m.Chunks = chunks
		case "/tts":
			var tts bool
			if strictJSON(op.Value, &tts) != nil {
				return m, false
			}
			m.TTS = &tts
		default:
			return m, false
		}
	}
	return m, (message{Service: m.Service, Channel: m.Channel, User: *m.User, Chunks: m.Chunks, TTS: *m.TTS}).valid()
}

func (s *server) listV3(ctx context.Context, w http.ResponseWriter, r *http.Request, owner int64) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, 400, "invalid_filter")
		return
	}
	ids := make(map[string]string)
	for _, key := range []string{"userId", "channelId", "replyTo"} {
		if values, ok := q[key]; ok {
			if len(values) != 1 {
				failure(w, 400, "invalid_filter")
				return
			}
			if _, ok := resourceID(values[0]); !ok {
				failure(w, 400, "invalid_filter")
				return
			}
			ids[key] = values[0]
			q.Del(key)
		}
	}
	f, err := parseFilter(q)
	if err != nil {
		failure(w, 400, "invalid_filter")
		return
	}
	all, err := s.graph.messagesV3(ctx, owner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	result := make([]messageResource, 0)
	for id, m := range all {
		if m.Deleted || !f.from.IsZero() && m.Time.Before(f.from) || !f.to.IsZero() && !m.Time.Before(f.to) || f.service != "" && m.Service != f.service || f.channel != "" && m.Channel != f.channel || ids["userId"] != "" && m.UserID != ids["userId"] || ids["channelId"] != "" && m.ChannelID != ids["channelId"] || ids["replyTo"] != "" && (m.ReplyTo == nil || *m.ReplyTo != ids["replyTo"]) {
			continue
		}
		result = append(result, m.resource(id))
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if f.descending {
			a, b = b, a
		}
		if !a.Time.Equal(b.Time) {
			return a.Time.Before(b.Time)
		}
		ai, _ := resourceID(a.ID)
		bi, _ := resourceID(b.ID)
		return ai < bi
	})
	start := min(f.offset, len(result))
	end := start + min(f.count, len(result)-start)
	var next *int
	if end < len(result) {
		next = &end
	}
	respond(w, 200, map[string]any{"result": result[start:end], "total": len(result), "continue": next})
}
