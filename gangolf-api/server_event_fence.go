package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
)

func eventCorrelation(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == value
}

type eventFenceInput struct {
	StreamID  string `json:"streamId"`
	RequestID string `json:"requestId"`
}

func (s *server) eventFence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "method_not_allowed")
		return
	}
	token, query := credential(r)
	if token == "" || query || r.URL.RawQuery != "" {
		failure(w, 401, "unauthorized")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	var input eventFenceInput
	if err != nil || strictJSON(body, &input) != nil || !eventCorrelation(input.StreamID) || !eventCorrelation(input.RequestID) {
		failure(w, 400, "invalid_fence")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), eventReadTimeout)
	defer cancel()
	status, code := s.enqueueFence(ctx, r, token, input)
	if status != 0 {
		failure(w, status, code)
		return
	}
	respond(w, 200, input)
}

func (s *server) enqueueFence(ctx context.Context, r *http.Request, token string, input eventFenceInput) (int, string) {
	if lockResources(ctx) != nil {
		return 503, "storage_unavailable"
	}
	defer resourceLock.Unlock()
	read := r.Clone(ctx)
	u := *r.URL
	u.Path = "/api/v3/events"
	read.URL = &u
	read.Method = http.MethodGet
	filter := eventFilter{types: map[string]bool{"message.created": true, "message.updated": true}, service: "Twitch"}
	p, _, status, code := s.lockedEventAuthorization(ctx, read, filter, nil)
	if status != 0 {
		return status, code
	}
	h := s.eventHub()
	h.mu.Lock()
	defer h.mu.Unlock()
	var target *eventSubscription
	for sub := range h.subs {
		if sub.id == input.StreamID {
			target = sub
			break
		}
	}
	if target == nil || target.credential != sha256.Sum256([]byte(token)) || !reflect.DeepEqual(target.principal, p) || target.filter.service != "Twitch" || target.filter.channel != "" || !target.filter.types["message.created"] || !target.filter.types["message.updated"] {
		return 404, "not_found"
	}
	if target.invalid || h.closed || target.fence != "" {
		return 409, "stream_unavailable"
	}
	payload, _ := json.Marshal(map[string]any{"schemaVersion": 1, "requestId": input.RequestID})
	frame := eventFrame{fence: input.RequestID, bytes: append(append([]byte("event: stream.fence\ndata: "), payload...), '\n', '\n')}
	if len(target.queue) == eventQueue || target.bytes+len(frame.bytes) > eventBytes {
		h.removeLocked(target)
		return 409, "stream_unavailable"
	}
	target.fence = input.RequestID
	target.bytes += len(frame.bytes)
	target.queue <- frame
	return 0, ""
}
