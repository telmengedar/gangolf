package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

var errMediaCapacity = errors.New("catalog capacity exceeded")

func mediaFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, errMediaCapacity) {
		failure(w, 409, "catalog_capacity_exceeded")
	} else if errors.Is(err, errCorrupt) {
		failure(w, 503, "media_unavailable")
	} else {
		storageFailure(w, err)
	}
}
func (s *server) mediaHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v3/"), "/")
	item := len(parts) == 2
	id := ""
	if len(parts) > 2 || item && parts[1] == "" {
		failure(w, 404, "not_found")
		return
	}
	if item {
		id = parts[1]
		if _, ok := resourceID(id); !ok {
			failure(w, 400, "invalid_id")
			return
		}
	}
	allow := "GET, POST"
	valid := r.Method == "GET" || r.Method == "POST"
	if item {
		allow = "GET, PUT, DELETE"
		valid = r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE"
	}
	if !valid {
		w.Header().Set("Allow", allow)
		failure(w, 405, "method_not_allowed")
		return
	}
	token, query := credential(r)
	if token == "" || query {
		failure(w, 401, "unauthorized")
		return
	}
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		storageFailure(w, err)
		return
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
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, 400, "invalid_filter")
		return
	}
	filter, ok := parseMediaFilter(q, parts[0])
	if !ok || r.Method != "GET" && len(q) > 0 || item && len(q) > 0 {
		failure(w, 400, "invalid_filter")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.load(ctx)
	if err != nil {
		mediaFailure(w, err)
		return
	}
	if r.Method == "GET" && !item {
		mediaList(w, c, parts[0], filter)
		return
	}
	clipIndex, triggerIndex := -1, -1
	for i, v := range c.Clips {
		if v.ID == id {
			clipIndex = i
		}
	}
	for i, v := range c.Triggers {
		if v.ID == id {
			triggerIndex = i
		}
	}
	clips := parts[0] == "clips"
	index := triggerIndex
	if clips {
		index = clipIndex
	}
	if item && index < 0 {
		failure(w, 404, "not_found")
		return
	}
	if r.Method == "GET" {
		if clips {
			respond(w, 200, c.Clips[index])
		} else {
			respond(w, 200, c.Triggers[index])
		}
		return
	}
	if r.Method == "DELETE" {
		if clips {
			for _, t := range c.Triggers {
				if t.ClipID != nil && *t.ClipID == id {
					failure(w, 409, "clip_in_use")
					return
				}
			}
			c.Clips = append(c.Clips[:index], c.Clips[index+1:]...)
		} else {
			c.Triggers = append(c.Triggers[:index], c.Triggers[index+1:]...)
		}
		if err = m.save(ctx, c); err != nil {
			mediaFailure(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(204)
		return
	}
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" {
		failure(w, 415, "unsupported_media_type")
		return
	}
	body, ok := resourceBody(w, r)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	code := "invalid_trigger"
	if clips {
		code = "invalid_clip"
	}
	if mediaJSON(body, &fields) != nil || fields == nil {
		failure(w, 400, code)
		return
	}
	for key := range fields {
		allowed := key == "id" || key == "legacy"
		if clips {
			allowed = allowed || key == "provider" || key == "remoteVideoId" || key == "startSeconds" || key == "endSeconds" || key == "title"
		} else {
			allowed = allowed || key == "text" || key == "clipId"
		}
		if !allowed {
			failure(w, 400, code)
			return
		}
	}
	for _, key := range []string{"provider", "remoteVideoId"} {
		if !clips {
			break
		}
		if !nonNull(fields[key]) {
			failure(w, 400, code)
			return
		}
	}
	if !clips && (!nonNull(fields["text"]) || !nonNull(fields["clipId"])) {
		failure(w, 400, code)
		return
	}
	if !item && (fields["id"] != nil || fields["legacy"] != nil) {
		failure(w, 400, code)
		return
	}
	var value any
	if clips {
		var v mediaClip
		if mediaJSON(body, &v) != nil || !v.valid() {
			failure(w, 400, code)
			return
		}
		if item {
			old := c.Clips[index]
			if fields["id"] != nil && v.ID != old.ID || fields["legacy"] != nil && !reflect.DeepEqual(v.Legacy, old.Legacy) {
				failure(w, 400, code)
				return
			}
			v.ID, v.Legacy = old.ID, old.Legacy
			c.Clips[index] = v
		} else {
			if c.NextClipID == math.MaxInt64 {
				mediaFailure(w, errMediaCapacity)
				return
			}
			v.ID = strconv.FormatInt(c.NextClipID, 10)
			c.NextClipID++
			c.Clips = append(c.Clips, v)
		}
		value = v
	} else {
		var v mediaTrigger
		if mediaJSON(body, &v) != nil || !validMediaText(v.Text) || v.ClipID == nil {
			failure(w, 400, code)
			return
		}
		if _, ok := resourceID(*v.ClipID); !ok {
			failure(w, 400, code)
			return
		}
		found := false
		for _, clip := range c.Clips {
			if clip.ID == *v.ClipID {
				found = true
			}
		}
		if !found {
			failure(w, 404, "clip_not_found")
			return
		}
		if item {
			old := c.Triggers[index]
			if fields["id"] != nil && v.ID != old.ID || fields["legacy"] != nil && !reflect.DeepEqual(v.Legacy, old.Legacy) {
				failure(w, 400, code)
				return
			}
			v.ID, v.Legacy = old.ID, old.Legacy
			c.Triggers[index] = v
		} else {
			if c.NextTriggerID == math.MaxInt64 {
				mediaFailure(w, errMediaCapacity)
				return
			}
			v.ID = strconv.FormatInt(c.NextTriggerID, 10)
			c.NextTriggerID++
			c.Triggers = append(c.Triggers, v)
		}
		value = v
	}
	if err = m.save(ctx, c); err != nil {
		mediaFailure(w, err)
		return
	}
	status = 200
	if !item {
		status = 201
		newID := ""
		if clips {
			newID = value.(mediaClip).ID
		} else {
			newID = value.(mediaTrigger).ID
		}
		w.Header().Set("Location", "/api/v3/"+parts[0]+"/"+newID)
	}
	respond(w, status, value)
}

type mediaFilter struct {
	count, offset                    int
	descending                       bool
	provider, remoteID, clipID, text *string
	unresolved                       *bool
}

func parseMediaFilter(q url.Values, kind string) (mediaFilter, bool) {
	f := mediaFilter{count: 100}
	for k, v := range q {
		if len(v) != 1 {
			return f, false
		}
		switch k {
		case "count", "continue":
			n, err := strconv.Atoi(v[0])
			if err != nil || n < 0 || k == "count" && (n == 0 || n > 500) {
				return f, false
			}
			if k == "count" {
				f.count = n
			} else {
				f.offset = n
			}
		case "sort":
			if v[0] != "id" {
				return f, false
			}
		case "descending", "unresolved":
			if v[0] != "true" && v[0] != "false" {
				return f, false
			}
			b := v[0] == "true"
			if k == "descending" {
				f.descending = b
			} else {
				if kind != "triggers" {
					return f, false
				}
				f.unresolved = &b
			}
		case "provider":
			if kind != "clips" {
				return f, false
			}
			f.provider = &v[0]
		case "remoteVideoId":
			if kind != "clips" {
				return f, false
			}
			f.remoteID = &v[0]
		case "text":
			if kind != "triggers" {
				return f, false
			}
			f.text = &v[0]
		case "clipId":
			if kind != "triggers" {
				return f, false
			}
			if _, ok := resourceID(v[0]); !ok {
				return f, false
			}
			f.clipID = &v[0]
		default:
			return f, false
		}
	}
	return f, true
}
func mediaList(w http.ResponseWriter, c mediaCatalog, kind string, f mediaFilter) {
	ids := make([]int64, 0)
	values := map[int64]any{}
	if kind == "clips" {
		for _, v := range c.Clips {
			if f.provider != nil && v.Provider != *f.provider || f.remoteID != nil && v.RemoteVideoID != *f.remoteID {
				continue
			}
			id, _ := resourceID(v.ID)
			ids = append(ids, id)
			values[id] = v
		}
	} else {
		for _, v := range c.Triggers {
			if f.text != nil && v.Text != *f.text || f.clipID != nil && (v.ClipID == nil || *v.ClipID != *f.clipID) || f.unresolved != nil && *f.unresolved != (v.ClipID == nil) {
				continue
			}
			id, _ := resourceID(v.ID)
			ids = append(ids, id)
			values[id] = v
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if f.descending {
			return ids[i] > ids[j]
		}
		return ids[i] < ids[j]
	})
	start := min(f.offset, len(ids))
	end := start + min(f.count, len(ids)-start)
	result := make([]any, 0, end-start)
	for _, id := range ids[start:end] {
		result = append(result, values[id])
	}
	var next *int
	if end < len(ids) {
		next = &end
	}
	respond(w, 200, map[string]any{"result": result, "total": len(ids), "continue": next})
}
