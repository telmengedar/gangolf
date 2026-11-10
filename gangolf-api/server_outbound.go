package main

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
)

func deliveryFailure(w http.ResponseWriter, status int, code string, id int64) {
	body := map[string]string{"code": code}
	if id > 0 {
		body["id"] = strconv.FormatInt(id, 10)
		w.Header().Set("Location", deliveryLocation(id))
	}
	respond(w, status, body)
}

func (s *server) replayOutbound(ctx context.Context, w http.ResponseWriter, input messageInput, name string, owner int64, policy messagePolicy) bool {
	n, m, found, err := s.graph.outboundLookup(ctx, name, owner)
	if err != nil {
		storageFailure(w, err)
		return true
	}
	if !found {
		return false
	}
	visible, err := policy.visibleV3(ctx, s.graph, m, owner)
	if err != nil {
		storageFailure(w, err)
		return true
	}
	if !visible {
		failure(w, 403, "forbidden")
		return true
	}
	w.Header().Set("Location", deliveryLocation(n.ID))
	if m.Deleted {
		deliveryFailure(w, 410, "message_deleted", n.ID)
	} else if !sameOutboundRequest(m, input) {
		failure(w, 409, "idempotency_conflict")
	} else {
		respond(w, 200, m.resource(n.ID))
	}
	return true
}

func (s *server) createTwitchOutbound(ctx context.Context, w http.ResponseWriter, r *http.Request, input messageInput, cfg *authConfig, policy messagePolicy) {
	text, code := outboundText(input)
	if code != "" {
		failure(w, 422, code)
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !validRequestKey(keys[0]) {
		failure(w, 400, "invalid_idempotency_key")
		return
	}
	name := outboundName(policy.authID, policy.principal.UserID, keys[0])
	if s.replayOutbound(ctx, w, input, name, cfg.serviceOwner, policy) {
		return
	}
	p, ok := selectOutbound(cfg, s.graph.config, policy.principal, input.Channel)
	if !ok {
		failure(w, 403, "forbidden")
		return
	}
	if s.twitch == nil || s.twitch.client == nil {
		failure(w, 503, "delivery_unavailable")
		return
	}
	resourceLock.Unlock()
	botCredential, err := s.twitch.client.authorizeSend(ctx, p)
	resourceLock.Lock()
	if err != nil {
		failure(w, 503, "delivery_credentials_unavailable")
		return
	}
	current, err := s.graph.loadConfig(ctx)
	if err != nil {
		failure(w, 503, "configuration_unavailable")
		return
	}
	token, query := credential(r)
	caller, status := s.authorize(ctx, r, current, token, query)
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
	if caller.UserID != policy.principal.UserID || current.serviceOwner != cfg.serviceOwner {
		failure(w, 403, "forbidden")
		return
	}
	policy = s.policy(caller, current)
	if s.replayOutbound(ctx, w, input, name, current.serviceOwner, policy) {
		return
	}
	selected, ok := selectOutbound(current, s.graph.config, caller, input.Channel)
	if !ok || !sameReceiver(selected, p) {
		failure(w, 403, "forbidden")
		return
	}
	uid, err := s.graph.resolveIdentity(ctx, "user", identityContent{SchemaVersion: 1, Service: "Twitch", ServiceID: "GangolfBot", Name: "GangolfBot"}, current.serviceOwner)
	if err != nil {
		storageFailure(w, err)
		return
	}
	m := messageContent{SchemaVersion: 3, Delivery: &twitchDelivery{RequestKey: keys[0], AuthUserID: caller.UserID, ConfigID: strconv.FormatInt(p.id, 10), State: "unknown"}, UserID: strconv.FormatInt(uid, 10), ChannelID: p.ChannelID, Time: s.now().UTC(), Service: input.Service, Channel: input.Channel, User: &input.User, Chunks: input.Chunks, TTS: &input.TTS}
	id, err := s.graph.reserveOutbound(ctx, name, m, current.serviceOwner)
	if err != nil {
		deliveryFailure(w, 503, "delivery_outcome_unknown", id)
		return
	}
	w.Header().Set("Location", deliveryLocation(id))
	resourceLock.Unlock()
	state, messageID, resultCode, retry := s.twitch.client.sendMessage(ctx, p, botCredential, text)
	resourceLock.Lock()
	if state == "unknown" {
		deliveryFailure(w, 503, "delivery_outcome_unknown", id)
		return
	}
	n, stored, found, err := s.graph.outboundLookup(ctx, name, current.serviceOwner)
	if err != nil || !found || n.ID != id || !reflect.DeepEqual(stored.Delivery, m.Delivery) || !stored.Deleted && !sameOutboundRequest(stored, input) {
		deliveryFailure(w, 503, "delivery_outcome_unknown", id)
		return
	}
	stored.Delivery = &twitchDelivery{RequestKey: keys[0], AuthUserID: caller.UserID, ConfigID: strconv.FormatInt(p.id, 10), State: state, MessageID: messageID, Code: resultCode}
	if err := s.graph.replaceContent(ctx, id, stored); err != nil {
		deliveryFailure(w, 503, "delivery_outcome_unknown", id)
		return
	}
	_, confirmed, found, err := s.graph.outboundLookup(ctx, name, current.serviceOwner)
	if err != nil || !found || !reflect.DeepEqual(stored, confirmed) {
		deliveryFailure(w, 503, "delivery_outcome_unknown", id)
		return
	}
	if state == "failed" {
		if resultCode == "twitch_rate_limited" {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			deliveryFailure(w, 429, "delivery_rate_limited", id)
		} else {
			deliveryFailure(w, 502, "delivery_failed", id)
		}
		return
	}
	m.Delivery = stored.Delivery
	respond(w, 201, m.resource(id))
}
