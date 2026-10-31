package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

var errUnavailable = errors.New("dependency unavailable")
var errCorrupt = errors.New("corrupt graph state")
var errOutcomeUnknown = errors.New("write outcome unknown")

const batchType = "gangolf-message-batch"

type graph struct {
	base                *url.URL
	token               string
	root, group, config int64
	groupMu             sync.RWMutex
	groups              map[string]int64
	http                *http.Client
}

type node struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Owner       int64           `json:"ownerId"`
	Links       []relation      `json:"linkDetails"`
	Type        string          `json:"type"`
	TypeIsNull  bool            `json:"-"`
	Root        int64           `json:"rootNodeId"`
	Access      json.RawMessage `json:"access"`
	Content     *string         `json:"content"`
	ContentType string          `json:"contentType"`
}

func (n *node) UnmarshalJSON(data []byte) error {
	type nodeAlias node
	if err := json.Unmarshal(data, (*nodeAlias)(n)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	typeValue, present := fields["type"]
	n.TypeIsNull = present && bytes.Equal(bytes.TrimSpace(typeValue), []byte("null"))
	return nil
}

func (n node) MarshalJSON() ([]byte, error) {
	type nodeAlias node
	data, err := json.Marshal(nodeAlias(n))
	if err != nil || n.Type != "" {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	fields["type"] = json.RawMessage("null")
	return json.Marshal(fields)
}

func (n node) privateAccess() bool {
	if bytes.Equal(bytes.TrimSpace(n.Access), []byte("0")) {
		return true
	}
	var name string
	return json.Unmarshal(n.Access, &name) == nil && name == "None"
}

type graphPage struct {
	Result   []node `json:"result"`
	Total    int    `json:"total"`
	Continue *int   `json:"continue"`
}

func (g *graph) request(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	u := *g.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := g.http.Do(req)
	if err != nil {
		return nil, errUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, errUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20+1))
	if err != nil || len(data) > 32<<20 {
		return nil, errUnavailable
	}
	return data, nil
}

func (g *graph) loadConfig(ctx context.Context) (*authConfig, error) {
	owner, err := g.serviceIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if err := g.privateContainers(ctx, owner); err != nil {
		return nil, err
	}
	data, err := g.request(ctx, http.MethodGet, "/nodes/"+strconv.FormatInt(g.config, 10), nil, nil)
	if err != nil {
		return nil, err
	}
	var n node
	if json.Unmarshal(data, &n) != nil || n.ID != g.config || !g.privateNode(n, owner) {
		return nil, errCorrupt
	}
	data, err = g.request(ctx, http.MethodGet, "/nodes/"+strconv.FormatInt(g.config, 10)+"/content", nil, nil)
	if err != nil {
		return nil, err
	}
	var cfg authConfig
	if json.Unmarshal(data, &cfg) != nil || !cfg.valid() {
		return nil, errCorrupt
	}
	cfg.serviceOwner = owner
	return &cfg, nil
}

func (g *graph) append(ctx context.Context, messages []message) (int64, error) {
	payload, _ := json.Marshal(batch{SchemaVersion: 1, Messages: messages})
	metadata, _ := json.Marshal(map[string]any{"name": "message batch", "type": batchType, "rootNodeId": g.root, "access": 0, "skipEmbedding": true})
	data, err := g.request(ctx, http.MethodPost, "/nodes", nil, metadata)
	if err != nil {
		return 0, err
	}
	var created node
	if json.Unmarshal(data, &created) != nil || created.ID <= 0 || created.Root != g.root || !created.privateAccess() || created.Type != batchType {
		return 0, errCorrupt
	}
	path := "/nodes/" + strconv.FormatInt(created.ID, 10)
	link, _ := json.Marshal(g.group)
	if _, err = g.request(ctx, http.MethodPost, path+"/links", nil, link); err != nil {
		return 0, err
	}
	if _, err = g.request(ctx, http.MethodPost, path+"/content", nil, payload); err != nil {
		return 0, errOutcomeUnknown
	}
	return created.ID, nil
}

func (g *graph) list(ctx context.Context) ([]storedMessage, error) {
	result := make([]storedMessage, 0)
	offset := 0
	var lastID int64
	for {
		q := url.Values{"rootNodeId": {strconv.FormatInt(g.root, 10)}, "linkedto": {strconv.FormatInt(g.group, 10)}, "type": {batchType}, "fields": {"id,type,rootNodeId,access,content,contentType"}, "sort": {"id"}, "descending": {"false"}, "count": {"500"}, "continue": {strconv.Itoa(offset)}}
		data, err := g.request(ctx, http.MethodGet, "/nodes", q, nil)
		if err != nil {
			return nil, err
		}
		var page graphPage
		if json.Unmarshal(data, &page) != nil || page.Total < 0 || page.Result == nil {
			return nil, errCorrupt
		}
		for _, n := range page.Result {
			if n.ID <= lastID || n.Root != g.root || n.Type != batchType || !n.privateAccess() {
				return nil, errCorrupt
			}
			lastID = n.ID
			if n.Content == nil {
				continue
			}
			if !strings.HasPrefix(n.ContentType, "application/json") {
				return nil, errCorrupt
			}
			var b batch
			if strictJSON([]byte(*n.Content), &b) != nil || b.SchemaVersion != 1 || len(b.Messages) == 0 || len(b.Messages) > 100 {
				return nil, errCorrupt
			}
			for i, m := range b.Messages {
				if !m.valid() || m.Time.IsZero() || m.Time.Location() != timeUTC || nonNull(m.PostTo) {
					return nil, errCorrupt
				}
				result = append(result, storedMessage{message: m, batchID: n.ID, index: i})
			}
		}
		next := offset + len(page.Result)
		if page.Continue != nil {
			if *page.Continue != next || next <= offset {
				return nil, errCorrupt
			}
			offset = next
		} else if next < page.Total {
			if next <= offset {
				return nil, errCorrupt
			}
			offset = next
		} else {
			return result, nil
		}
	}
}
