package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type server struct {
	graph  *graph
	pepper string
	now    func() time.Time
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]string{"code": code, "text": strings.ReplaceAll(code, "_", " ")})
}

func storageFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCorrupt):
		failure(w, 500, "corrupt_storage")
	case errors.Is(err, errOutcomeUnknown):
		failure(w, 503, "write_outcome_unknown")
	default:
		failure(w, 503, "storage_unavailable")
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if r.Method == http.MethodGet && r.URL.Path == "/api/i1/health" {
		respond(w, 200, map[string]string{"status": "live"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/i1/readiness" {
		_, err := s.graph.loadConfig(ctx)
		if err == nil {
			_, err = s.graph.list(ctx)
		}
		if err != nil {
			respond(w, 503, map[string]string{"status": "unready"})
			return
		}
		respond(w, 200, map[string]string{"status": "ready"})
		return
	}
	if r.URL.Path != "/api/v2/messages" {
		failure(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		failure(w, 405, "method_not_allowed")
		return
	}
	token, queryKey := credential(r)
	if token == "" {
		failure(w, 401, "unauthorized")
		return
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		storageFailure(w, err)
		return
	}
	if status := s.authorize(ctx, r, cfg, token, queryKey); status != 0 {
		switch status {
		case 503:
			failure(w, status, "auth_unavailable")
		case 403:
			failure(w, status, "forbidden")
		default:
			failure(w, status, "unauthorized")
		}
		return
	}
	if r.Method == http.MethodPost {
		s.post(ctx, w, r)
		return
	}
	s.get(ctx, w, r)
}

func (s *server) post(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			failure(w, 413, "body_too_large")
		} else {
			failure(w, 400, "invalid_body")
		}
		return
	}
	var messages []message
	if strictJSON(body, &messages) != nil || len(messages) == 0 || len(messages) > 100 {
		failure(w, 400, "invalid_batch")
		return
	}
	for _, m := range messages {
		if nonNull(m.PostTo) {
			failure(w, 422, "delivery_not_supported")
			return
		}
		if !m.valid() {
			failure(w, 400, "invalid_message")
			return
		}
	}
	now := s.now().UTC()
	for i := range messages {
		messages[i].Time = now
		messages[i].PostTo = nil
	}
	id, err := s.graph.append(ctx, messages)
	if err != nil {
		storageFailure(w, err)
		return
	}
	respond(w, 201, map[string]any{"batchId": id, "result": messages})
}

type filter struct {
	from, to         time.Time
	service, channel string
	count, offset    int
	descending       bool
}

func parseFilter(q url.Values) (filter, error) {
	f := filter{count: 100, service: q.Get("service"), channel: q.Get("channel")}
	for k, v := range q {
		if len(v) != 1 {
			return f, errCorrupt
		}
		switch k {
		case "from", "to", "service", "channel", "count", "continue", "sort", "descending", "apiKey":
		default:
			return f, errCorrupt
		}
	}
	if q.Has("sort") && q.Get("sort") != "time" {
		return f, errCorrupt
	}
	var err error
	if q.Has("count") {
		f.count, err = strconv.Atoi(q.Get("count"))
		if err != nil || f.count < 1 || f.count > 500 {
			return f, errCorrupt
		}
	}
	if q.Has("continue") {
		f.offset, err = strconv.Atoi(q.Get("continue"))
		if err != nil || f.offset < 0 {
			return f, errCorrupt
		}
	}
	if q.Has("descending") {
		f.descending, err = strconv.ParseBool(q.Get("descending"))
		if err != nil {
			return f, errCorrupt
		}
	}
	if q.Has("from") {
		f.from, err = time.Parse(time.RFC3339Nano, q.Get("from"))
		if err != nil {
			return f, errCorrupt
		}
	}
	if q.Has("to") {
		f.to, err = time.Parse(time.RFC3339Nano, q.Get("to"))
		if err != nil {
			return f, errCorrupt
		}
	}
	if !f.from.IsZero() && !f.to.IsZero() && !f.from.Before(f.to) {
		return f, errCorrupt
	}
	return f, nil
}

func (s *server) get(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, 400, "invalid_filter")
		return
	}
	f, err := parseFilter(q)
	if err != nil {
		failure(w, 400, "invalid_filter")
		return
	}
	all, err := s.graph.list(ctx)
	if err != nil {
		storageFailure(w, err)
		return
	}
	matches := make([]storedMessage, 0)
	for _, m := range all {
		if !f.from.IsZero() && m.Time.Before(f.from) || !f.to.IsZero() && !m.Time.Before(f.to) || f.service != "" && m.Service != f.service || f.channel != "" && m.Channel != f.channel {
			continue
		}
		matches = append(matches, m)
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if f.descending {
			a, b = b, a
		}
		if !a.Time.Equal(b.Time) {
			return a.Time.Before(b.Time)
		}
		if a.batchID != b.batchID {
			return a.batchID < b.batchID
		}
		return a.index < b.index
	})
	start := min(f.offset, len(matches))
	end := start + min(f.count, len(matches)-start)
	result := make([]message, 0, end-start)
	for _, m := range matches[start:end] {
		result = append(result, m.message)
	}
	var next *int
	if end < len(matches) {
		next = &end
	}
	respond(w, 200, map[string]any{"result": result, "total": len(matches), "continue": next})
}
