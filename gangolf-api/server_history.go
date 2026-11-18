package main

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"sort"
)

func (s *server) authorizeV3(ctx context.Context, w http.ResponseWriter, r *http.Request) (*authConfig, principal, bool) {
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		storageFailure(w, err)
		return nil, principal{}, false
	}
	token, query := credential(r)
	p, status := s.authorize(ctx, r, cfg, token, query)
	if status != 0 {
		code := "unauthorized"
		if status == 403 {
			code = "forbidden"
		} else if status == 503 {
			code = "auth_unavailable"
		}
		failure(w, status, code)
		return nil, principal{}, false
	}
	return cfg, p, true
}

func (s *server) listV3(ctx context.Context, w http.ResponseWriter, r *http.Request) {
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
	for attempt := 0; attempt < 2; attempt++ {
		if err := lockResources(ctx); err != nil {
			storageFailure(w, err)
			return
		}
		cfg, p, ok := s.authorizeV3(ctx, w, r)
		epoch := s.graph.mutationEpoch.Load()
		resourceLock.Unlock()
		if !ok {
			return
		}
		memo := &graphReadMemo{graph: s.graph, responses: make(map[string][]byte)}
		scan := context.WithValue(ctx, graphReadMemoKey, memo)
		result, scanErr := s.historyV3(scan, cfg.serviceOwner, s.policy(p, cfg), f, ids)
		if err := lockResources(ctx); err != nil {
			storageFailure(w, err)
			return
		}
		current, caller, ok := s.authorizeV3(ctx, w, r)
		stable := ok && epoch == s.graph.mutationEpoch.Load() && reflect.DeepEqual(cfg, current) && reflect.DeepEqual(p, caller)
		resourceLock.Unlock()
		if !ok {
			return
		}
		if !stable {
			continue
		}
		if scanErr != nil {
			storageFailure(w, scanErr)
			return
		}
		respond(w, 200, result)
		return
	}
	storageFailure(w, errUnavailable)
}

func (s *server) historyV3(ctx context.Context, owner int64, policy messagePolicy, f filter, ids map[string]string) (map[string]any, error) {
	all, err := s.graph.messagesV3(ctx, owner)
	if err != nil {
		return nil, err
	}
	result := make([]messageResource, 0)
	for id, m := range all {
		visible, accessErr := policy.allowed(ctx, m.Service, m.Channel, m.ChannelID, false)
		if accessErr != nil {
			return nil, accessErr
		}
		if !visible {
			continue
		}
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
	return map[string]any{"result": result[start:end], "total": len(result), "continue": next}, nil
}
