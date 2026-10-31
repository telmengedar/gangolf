package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
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

func domainGroup(kind string) (string, bool) {
	switch kind {
	case "user":
		return "Users", true
	case "channel":
		return "Channels", true
	case "message":
		return "Messages", true
	default:
		return "", false
	}
}

func (g *graph) groupID(kind string) (int64, bool) {
	if _, ok := domainGroup(kind); !ok {
		return 0, false
	}
	g.groupMu.RLock()
	id, ok := g.groups[kind]
	g.groupMu.RUnlock()
	return id, ok && id > 0
}

func plainLinkBetween(links []relation, a, b int64) bool {
	for _, e := range links {
		if (e.Source == a && e.Target == b || e.Source == b && e.Target == a) && e.Type == "None" && e.Context == "" {
			return true
		}
	}
	return false
}

func exactMembership(n node, group int64) bool {
	memberships := 0
	for _, e := range n.Links {
		if (e.Source == n.ID && e.Target == group || e.Target == n.ID && e.Source == group) && e.Type == "None" && e.Context == "" {
			memberships++
			continue
		}
		if e.Type == "Unidirectional" {
			if n.Type == "user" && e.Target == n.ID && e.Context == "authored-by" || n.Type == "channel" && e.Target == n.ID && e.Context == "in-channel" {
				continue
			}
			if n.Type == "message" && (e.Source == n.ID && (e.Context == "authored-by" || e.Context == "in-channel" || e.Context == "replies-to") || e.Target == n.ID && e.Context == "replies-to") {
				continue
			}
		}
		return false
	}
	return memberships == 1
}

func (g *graph) groupByName(ctx context.Context, name string, owner int64) (node, error) {
	result := make([]node, 0, 1)
	offset := 0
	for {
		q := url.Values{"rootNodeId": {strconv.FormatInt(g.root, 10)}, "name": {name}, "fields": {"id,name,type,rootNodeId,ownerId,access,linkDetails"}, "sort": {"id"}, "descending": {"false"}, "count": {"500"}, "continue": {strconv.Itoa(offset)}}
		data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
		if err != nil {
			return node{}, err
		}
		var page graphPage
		if json.Unmarshal(data, &page) != nil || page.Result == nil || page.Total < 0 {
			return node{}, errCorrupt
		}
		for _, n := range page.Result {
			if n.Name == name {
				result = append(result, n)
			}
		}
		next := offset + len(page.Result)
		if page.Continue != nil && (*page.Continue != next || next <= offset) {
			return node{}, errCorrupt
		}
		if page.Continue == nil && next >= page.Total {
			break
		}
		if next <= offset {
			return node{}, errCorrupt
		}
		offset = next
	}
	if len(result) != 1 {
		return node{}, errCorrupt
	}
	n := result[0]
	if n.ID <= 0 || n.Root != g.root || n.Type != "" || !n.TypeIsNull || n.Owner != owner || !n.privateAccess() || !plainLinkBetween(n.Links, n.ID, g.root) {
		return node{}, errCorrupt
	}
	return n, nil
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

func (g *graph) privateDomainGroups(ctx context.Context, owner int64) error {
	q := url.Values{"id": {strconv.FormatInt(g.root, 10)}, "fields": {"id,name,type,ownerId,access,linkDetails"}, "count": {"2"}}
	data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
	if err != nil {
		return err
	}
	var projectPage graphPage
	if json.Unmarshal(data, &projectPage) != nil || projectPage.Result == nil || projectPage.Total != 1 || len(projectPage.Result) != 1 || projectPage.Continue != nil {
		return errCorrupt
	}
	project := projectPage.Result[0]
	if project.ID != g.root || project.Type != "project" || project.Name != "Gangolf" || project.Owner != owner || !project.privateAccess() {
		return errCorrupt
	}
	resolved := make(map[string]int64, 3)
	for _, kind := range []string{"user", "channel", "message"} {
		name, _ := domainGroup(kind)
		container, err := g.groupByName(ctx, name, owner)
		if err != nil {
			return err
		}
		if !plainLinkBetween(project.Links, g.root, container.ID) {
			return errCorrupt
		}
		if kind == "message" && container.ID != g.group {
			return errCorrupt
		}
		for _, existing := range resolved {
			if existing == container.ID {
				return errCorrupt
			}
		}
		resolved[kind] = container.ID
	}
	g.groupMu.Lock()
	g.groups = resolved
	g.groupMu.Unlock()
	return nil
}

func nodePath(id int64) string {
	return "/nodes/" + strconv.FormatInt(id, 10)
}

func identityName(kind, service, key string) string {
	data, _ := json.Marshal([]string{service, key})
	return "gangolf-" + kind + ":" + base64.RawURLEncoding.EncodeToString(data)
}

func (g *graph) resourceNodes(ctx context.Context, kind, name string, owner int64) ([]node, error) {
	group, ok := g.groupID(kind)
	if !ok {
		return nil, errCorrupt
	}
	return g.queryResourceNodes(ctx, kind, name, owner, group)
}

func (g *graph) identityIntegrity(ctx context.Context, kind, name string, owner int64) ([]node, error) {
	if (kind != "user" && kind != "channel") || name == "" {
		return nil, errCorrupt
	}
	return g.queryResourceNodes(ctx, kind, name, owner, 0)
}

func (g *graph) queryResourceNodes(ctx context.Context, kind, name string, owner, group int64) ([]node, error) {
	result := make([]node, 0)
	offset, total := 0, -1
	var last int64
	for {
		q := url.Values{"rootNodeId": {strconv.FormatInt(g.root, 10)}, "type": {kind}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails"}, "sort": {"id"}, "descending": {"false"}, "count": {"500"}, "continue": {strconv.Itoa(offset)}}
		if group > 0 {
			q.Set("linkedto", strconv.FormatInt(group, 10))
		}
		if name != "" {
			q.Set("name", name)
		}
		data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Result   []node `json:"result"`
			Total    *int   `json:"total"`
			Continue *int   `json:"continue"`
		}
		if json.Unmarshal(data, &page) != nil || page.Result == nil || page.Total == nil || *page.Total < 0 || total >= 0 && total != *page.Total {
			return nil, errCorrupt
		}
		total = *page.Total
		for _, n := range page.Result {
			if n.ID <= last || n.Type != kind || !g.privateNode(n, owner) || n.Links == nil || name != "" && n.Name != name || group > 0 && !exactMembership(n, group) {
				return nil, errCorrupt
			}
			last = n.ID
			result = append(result, n)
		}
		next := offset + len(page.Result)
		if next > total || (page.Continue != nil) != (next < total) || page.Continue != nil && (*page.Continue != next || next <= offset) {
			return nil, errCorrupt
		}
		if next == total {
			return result, nil
		}
		offset = next
	}
}

func (g *graph) createResource(ctx context.Context, kind, name string, owner int64) (node, error) {
	group, ok := g.groupID(kind)
	if !ok {
		return node{}, errCorrupt
	}
	metadata, _ := json.Marshal(map[string]any{"name": name, "type": kind, "rootNodeId": g.root, "access": 0, "skipEmbedding": true})
	data, err := g.request(ctx, http.MethodPost, "/nodes", nil, metadata)
	if err != nil {
		return node{}, err
	}
	var n node
	if json.Unmarshal(data, &n) != nil || n.Type != kind || n.Name != name || !g.privateNode(n, owner) || n.Content != nil {
		return node{}, errCorrupt
	}
	if err := g.addPlainLink(ctx, n.ID, group); err != nil {
		return node{}, err
	}
	return n, nil
}

func (g *graph) addPlainLink(ctx context.Context, source, target int64) error {
	body, _ := json.Marshal(target)
	_, err := g.request(ctx, http.MethodPost, nodePath(source)+"/links", url.Values{"linkType": {"None"}}, body)
	return err
}

func (g *graph) replaceContent(ctx context.Context, id int64, value any) error {
	data, _ := json.Marshal(value)
	_, err := g.request(ctx, http.MethodPost, nodePath(id)+"/content", nil, data)
	return err
}

func canonicalIdentityProjection(n node) node {
	n.Access = json.RawMessage("0")
	n.Links = slices.Clone(n.Links)
	slices.SortFunc(n.Links, func(a, b relation) int {
		if order := cmp.Compare(a.Source, b.Source); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Target, b.Target); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Type, b.Type); order != 0 {
			return order
		}
		return cmp.Compare(a.Context, b.Context)
	})
	return n
}

func sameIdentityProjection(a, b node) bool {
	return a.privateAccess() && b.privateAccess() && reflect.DeepEqual(canonicalIdentityProjection(a), canonicalIdentityProjection(b))
}

func (g *graph) resolveIdentity(ctx context.Context, kind string, value identityContent, owner int64) (int64, error) {
	key := value.ServiceID
	if kind == "channel" {
		key = value.Channel
	}
	name := identityName(kind, value.Service, key)
	if kind != "user" && kind != "channel" {
		return 0, errCorrupt
	}
	nodes, err := g.resourceNodes(ctx, kind, name, owner)
	if err != nil {
		return 0, err
	}
	if len(nodes) > 1 {
		return 0, errCorrupt
	}
	if len(nodes) == 1 && nodes[0].Content != nil && *nodes[0].Content != "" {
		if _, err := identityValue(nodes[0], kind, value.Service, key); err != nil {
			return 0, err
		}
	}
	integrity, err := g.identityIntegrity(ctx, kind, name, owner)
	if err != nil {
		return 0, err
	}
	if len(integrity) > 1 || len(nodes) == 1 && (len(integrity) != 1 || !sameIdentityProjection(nodes[0], integrity[0])) {
		return 0, errCorrupt
	}
	var n node
	if len(nodes) == 1 {
		n = nodes[0]
	} else {
		if len(integrity) == 0 {
			n, err = g.createResource(ctx, kind, name, owner)
			if err != nil {
				return 0, err
			}
		} else {
			n = integrity[0]
			if n.Content != nil && *n.Content != "" || len(n.Links) != 0 {
				return 0, errCorrupt
			}
			group, ok := g.groupID(kind)
			if !ok {
				return 0, errCorrupt
			}
			if err := g.addPlainLink(ctx, n.ID, group); err != nil {
				return 0, err
			}
		}
		confirmed, err := g.resourceNodes(ctx, kind, name, owner)
		if err != nil {
			return 0, err
		}
		if len(confirmed) != 1 || confirmed[0].ID != n.ID {
			return 0, errCorrupt
		}
		n = confirmed[0]
	}
	confirmed, err := g.readEntity(ctx, n.ID, kind, owner)
	if err != nil {
		return 0, err
	}
	if !sameIdentityProjection(n, confirmed) {
		return 0, errCorrupt
	}
	n = confirmed
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
	group, ok := g.groupID(kind)
	if !ok {
		return node{}, errCorrupt
	}
	q := url.Values{"id": {strconv.FormatInt(id, 10)}, "rootNodeId": {strconv.FormatInt(g.root, 10)}, "linkedto": {strconv.FormatInt(group, 10)}, "type": {kind}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails"}, "count": {"2"}}
	data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
	if err != nil {
		return node{}, err
	}
	var page graphPage
	if json.Unmarshal(data, &page) != nil || page.Result == nil || page.Total != 1 || len(page.Result) != 1 || page.Continue != nil {
		return node{}, errCorrupt
	}
	n := page.Result[0]
	if n.ID != id || n.Type != kind || !g.privateNode(n, owner) || !exactMembership(n, group) {
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
	group, ok := g.groupID("message")
	if !ok {
		return errCorrupt
	}
	expected := map[string]int64{"authored-by": uid, "in-channel": cid}
	if m.ReplyTo != nil {
		expected["replies-to"], _ = resourceID(*m.ReplyTo)
	}
	for _, e := range n.Links {
		if e.Context == "" && e.Type == "None" && (e.Source == n.ID && e.Target == group || e.Target == n.ID && e.Source == group) {
			continue
		}
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
	links := []relation{{Source: n.ID, Target: uid, Type: "Unidirectional", Context: "authored-by"}, {Source: n.ID, Target: cid, Type: "Unidirectional", Context: "in-channel"}}
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
