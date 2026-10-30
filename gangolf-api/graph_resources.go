package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (g *graph) privateNode(n node, owner int64) bool {
	return n.ID > 0 && n.Root == g.root && n.Owner == owner && n.privateAccess()
}

func (g *graph) serviceIdentity(ctx context.Context) (int64, error) {
	data, err := g.request(ctx, http.MethodGet, "/users/me", nil, nil)
	if err != nil {
		return 0, err
	}
	var user struct {
		ID          int64    `json:"id"`
		Enabled     bool     `json:"enabled"`
		Permissions []string `json:"permissions"`
	}
	if json.Unmarshal(data, &user) != nil || user.ID <= 0 || !user.Enabled {
		return 0, errCorrupt
	}
	for _, p := range user.Permissions {
		if p == "write" || p == "admin" {
			return user.ID, nil
		}
	}
	return 0, errCorrupt
}

func (g *graph) privateContainers(ctx context.Context, owner int64) error {
	for _, id := range []int64{g.root, g.group} {
		data, err := g.request(ctx, http.MethodGet, nodePath(id), nil, nil)
		if err != nil {
			return err
		}
		var n node
		if json.Unmarshal(data, &n) != nil || n.ID != id || n.Owner != owner || !n.privateAccess() || id == g.group && n.Root != g.root {
			return errCorrupt
		}
	}
	return nil
}

func nodePath(id int64) string {
	return "/nodes/" + strconv.FormatInt(id, 10)
}

func identityName(kind, service, key string) string {
	data, _ := json.Marshal([]string{service, key})
	return "gangolf-" + kind + ":" + string(data)
}

func (g *graph) resourceNodes(ctx context.Context, kind, name string, owner int64) ([]node, error) {
	result := make([]node, 0)
	offset := 0
	var last int64
	for {
		q := url.Values{"rootNodeId": {strconv.FormatInt(g.root, 10)}, "type": {kind}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails"}, "sort": {"id"}, "descending": {"false"}, "count": {"500"}, "continue": {strconv.Itoa(offset)}}
		if kind == "message" {
			q.Set("linkedto", strconv.FormatInt(g.group, 10))
		}
		if name != "" {
			q.Set("name", name)
		}
		data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
		if err != nil {
			return nil, err
		}
		var page graphPage
		if json.Unmarshal(data, &page) != nil || page.Result == nil || page.Total < 0 {
			return nil, errCorrupt
		}
		for _, n := range page.Result {
			if n.ID <= last || n.Type != kind || !g.privateNode(n, owner) {
				return nil, errCorrupt
			}
			last = n.ID
			if name == "" || n.Name == name {
				result = append(result, n)
			}
		}
		next := offset + len(page.Result)
		if page.Continue != nil && (*page.Continue != next || next <= offset) {
			return nil, errCorrupt
		}
		if page.Continue == nil && next >= page.Total {
			return result, nil
		}
		if next <= offset {
			return nil, errCorrupt
		}
		offset = next
	}
}

func (g *graph) createResource(ctx context.Context, kind, name string, owner int64) (node, error) {
	metadata, _ := json.Marshal(map[string]any{"name": name, "type": kind, "rootNodeId": g.root, "access": 0, "skipEmbedding": true})
	data, err := g.request(ctx, http.MethodPost, "/nodes", nil, metadata)
	if err != nil {
		return node{}, err
	}
	var n node
	if json.Unmarshal(data, &n) != nil || n.Type != kind || n.Name != name || !g.privateNode(n, owner) || n.Content != nil {
		return node{}, errCorrupt
	}
	return n, nil
}

func (g *graph) replaceContent(ctx context.Context, id int64, value any) error {
	data, _ := json.Marshal(value)
	_, err := g.request(ctx, http.MethodPost, nodePath(id)+"/content", nil, data)
	return err
}

func (g *graph) resolveIdentity(ctx context.Context, kind string, value identityContent, owner int64) (int64, error) {
	key := value.ServiceID
	if kind == "channel" {
		key = value.Channel
	}
	name := identityName(kind, value.Service, key)
	nodes, err := g.resourceNodes(ctx, kind, name, owner)
	if err != nil {
		return 0, err
	}
	if len(nodes) > 1 {
		return 0, errCorrupt
	}
	var n node
	if len(nodes) == 0 {
		n, err = g.createResource(ctx, kind, name, owner)
		if err != nil {
			return 0, err
		}
	} else {
		n = nodes[0]
	}
	if n.Content == nil || *n.Content == "" {
		if err := g.replaceContent(ctx, n.ID, value); err != nil {
			return 0, err
		}
	} else if _, err := identityValue(n, kind, value.Service, key); err != nil {
		return 0, err
	}
	return n.ID, nil
}

func identityValue(n node, kind, service, key string) (identityContent, error) {
	var v identityContent
	if n.Content == nil || !strings.HasPrefix(n.ContentType, "application/json") || strictJSON([]byte(*n.Content), &v) != nil || v.SchemaVersion != 1 || v.Service != service || n.Name != identityName(kind, service, key) {
		return v, errCorrupt
	}
	if kind == "user" && (v.ServiceID != key || strings.TrimSpace(v.Name) == "" || v.Channel != "") || kind == "channel" && (v.Channel != key || v.ServiceID != "" || v.Name != "") {
		return v, errCorrupt
	}
	return v, nil
}

func (g *graph) readEntity(ctx context.Context, id int64, kind string, owner int64) (node, error) {
	q := url.Values{"id": {strconv.FormatInt(id, 10)}, "rootNodeId": {strconv.FormatInt(g.root, 10)}, "type": {kind}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails"}, "count": {"2"}}
	data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
	if err != nil {
		return node{}, err
	}
	var page graphPage
	if json.Unmarshal(data, &page) != nil || page.Result == nil || page.Total != 1 || len(page.Result) != 1 || page.Continue != nil {
		return node{}, errCorrupt
	}
	n := page.Result[0]
	if n.ID != id || n.Type != kind || !g.privateNode(n, owner) {
		return node{}, errCorrupt
	}
	return n, nil
}

func decodeMessage(n node) (messageContent, error) {
	var m messageContent
	if n.Content == nil {
		return m, nil
	}
	data := []byte(*n.Content)
	if !strings.HasPrefix(n.ContentType, "application/json") || strictJSON(data, &m) != nil || m.SchemaVersion != 1 || m.Time.IsZero() || m.Time.Location() != timeUTC {
		return m, errCorrupt
	}
	if _, ok := resourceID(m.UserID); !ok {
		return m, errCorrupt
	}
	if _, ok := resourceID(m.ChannelID); !ok {
		return m, errCorrupt
	}
	if m.ReplyTo != nil {
		if id, ok := resourceID(*m.ReplyTo); !ok || id >= n.ID {
			return m, errCorrupt
		}
	}
	if m.Deleted {
		tombstone, _ := json.Marshal(m)
		var actual, expected any
		json.Unmarshal(data, &actual)
		json.Unmarshal(tombstone, &expected)
		a, _ := json.Marshal(actual)
		b, _ := json.Marshal(expected)
		if m.Service != "" || m.Channel != "" || m.User != nil || m.Chunks != nil || m.TTS != nil || !bytes.Equal(a, b) {
			return m, errCorrupt
		}
	} else if m.User == nil || m.TTS == nil || !(message{Service: m.Service, Channel: m.Channel, User: *m.User, Chunks: m.Chunks, TTS: *m.TTS}).valid() {
		return m, errCorrupt
	}
	return m, nil
}

func (g *graph) validateRelations(ctx context.Context, n node, m messageContent, owner int64) error {
	uid, _ := resourceID(m.UserID)
	cid, _ := resourceID(m.ChannelID)
	expected := map[string]int64{"authored-by": uid, "in-channel": cid, "member-of": g.group}
	if m.ReplyTo != nil {
		expected["replies-to"], _ = resourceID(*m.ReplyTo)
	}
	for _, e := range n.Links {
		if e.Source != n.ID {
			if e.Target != n.ID || e.Context != "replies-to" || e.Type != "Unidirectional" {
				return errCorrupt
			}
			continue
		}
		target, ok := expected[e.Context]
		if !ok || e.Target != target || e.Type != "Unidirectional" {
			return errCorrupt
		}
		delete(expected, e.Context)
	}
	if len(expected) != 0 {
		return errCorrupt
	}
	for _, ref := range []struct {
		id   int64
		kind string
	}{{uid, "user"}, {cid, "channel"}} {
		entity, err := g.readEntity(ctx, ref.id, ref.kind, owner)
		if err != nil {
			return err
		}
		var v identityContent
		if entity.Content == nil || strictJSON([]byte(*entity.Content), &v) != nil {
			return errCorrupt
		}
		key := v.ServiceID
		if ref.kind == "channel" {
			key = v.Channel
		}
		if _, err := identityValue(entity, ref.kind, v.Service, key); err != nil || strings.TrimSpace(v.Service) == "" || strings.TrimSpace(key) == "" {
			return errCorrupt
		}
		if !m.Deleted && (ref.kind == "user" && (v.Service != m.User.Service || v.ServiceID != m.User.ServiceID) || ref.kind == "channel" && (v.Service != m.Service || v.Channel != m.Channel)) {
			return errCorrupt
		}
	}
	if m.ReplyTo != nil {
		pid, _ := resourceID(*m.ReplyTo)
		parent, err := g.readEntity(ctx, pid, "message", owner)
		if err != nil {
			return err
		}
		p, err := decodeMessage(parent)
		if err != nil || parent.Content == nil || p.ChannelID != m.ChannelID {
			return errCorrupt
		}
	}
	return nil
}

func (g *graph) messagesV3(ctx context.Context, owner int64) (map[int64]messageContent, error) {
	nodes, err := g.resourceNodes(ctx, "message", "", owner)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]messageContent)
	for _, n := range nodes {
		if n.Name != "message" {
			return nil, errCorrupt
		}
		if n.Content == nil {
			continue
		}
		m, err := decodeMessage(n)
		if err != nil {
			return nil, err
		}
		if err := g.validateRelations(ctx, n, m, owner); err != nil {
			return nil, err
		}
		result[n.ID] = m
	}
	for _, m := range result {
		if m.ReplyTo != nil {
			pid, _ := resourceID(*m.ReplyTo)
			if _, exists := result[pid]; !exists {
				return nil, errCorrupt
			}
		}
	}
	return result, nil
}

func (g *graph) publishMessage(ctx context.Context, m messageContent, owner int64) (int64, error) {
	n, err := g.createResource(ctx, "message", "message", owner)
	if err != nil {
		return 0, err
	}
	uid, _ := resourceID(m.UserID)
	cid, _ := resourceID(m.ChannelID)
	links := []relation{{Source: n.ID, Target: uid, Type: "Unidirectional", Context: "authored-by"}, {Source: n.ID, Target: cid, Type: "Unidirectional", Context: "in-channel"}, {Source: n.ID, Target: g.group, Type: "Unidirectional", Context: "member-of"}}
	if m.ReplyTo != nil {
		pid, _ := resourceID(*m.ReplyTo)
		links = append(links, relation{Source: n.ID, Target: pid, Type: "Unidirectional", Context: "replies-to"})
	}
	for _, e := range links {
		body, _ := json.Marshal(e.Target)
		if _, err := g.request(ctx, http.MethodPost, nodePath(n.ID)+"/links", url.Values{"linkType": {e.Type}, "context": {e.Context}}, body); err != nil {
			return 0, err
		}
	}
	confirmed, err := g.readEntity(ctx, n.ID, "message", owner)
	if err != nil {
		return 0, err
	}
	if err := g.validateRelations(ctx, confirmed, m, owner); err != nil {
		return 0, err
	}
	if err := g.replaceContent(ctx, n.ID, m); err != nil {
		return 0, errOutcomeUnknown
	}
	return n.ID, nil
}
