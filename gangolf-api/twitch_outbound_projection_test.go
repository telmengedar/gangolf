package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func honorIdentityFieldSelection(t *testing.T, s *server) *int {
	t.Helper()
	trimmed := new(int)
	observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/nodes" || q.Get("type") != "user" && q.Get("type") != "channel" || strings.Contains(","+q.Get("fields")+",", ",skipEmbedding,") {
			return
		}
		rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
			var rows []map[string]json.RawMessage
			if err := json.Unmarshal(page["result"], &rows); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if _, exists := row["skipEmbedding"]; exists {
					delete(row, "skipEmbedding")
					*trimmed++
				}
			}
			page["result"], _ = json.Marshal(rows)
		})
	})
	return trimmed
}

func TestCreateV3_FieldSelectedIdentityProjections_DeliversAndReplays(t *testing.T) {
	for _, mode := range []string{"new-author", "existing-author"} {
		t.Run(mode, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			n := f.nodes[31]
			n.SkipEmbedding = new(true)
			f.nodes[31] = n
			if mode == "existing-author" {
				cfg, err := s.graph.loadConfig(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.graph.resolveIdentity(context.Background(), "user", identityContent{SchemaVersion: 1, Service: "Twitch", ServiceID: "GangolfBot", Name: "GangolfBot"}, cfg.serviceOwner)
				if err != nil {
					t.Fatal(err)
				}
			}
			trimmed := honorIdentityFieldSelection(t, s)
			w := outboundRequest(s, "key.secret", "field-selection", outboundBody)
			if *trimmed == 0 {
				t.Fatal("field selection did not remove any unrequested metadata")
			}
			if w.Code != http.StatusCreated {
				t.Fatalf("field-selected POST status=%d body=%s fake_sends=%d", w.Code, w.Body.String(), sends(tw))
			}
			m := outboundResource(t, w, "sent")
			stored := storedOutbound(t, f, m.ID)
			if stored.SchemaVersion != 3 || stored.Delivery.State != "sent" || sends(tw) != 1 {
				t.Fatal("field-selected publication did not confirm delivery")
			}
			status(t, request(t, s, "GET", deliveryLocation(mustResourceID(m.ID)), "key.secret", ""), 200)
			replay := outboundRequest(s, "key.secret", "field-selection", outboundBody)
			status(t, replay, 200)
			if outboundResource(t, replay, "sent").ID != m.ID || sends(tw) != 1 {
				t.Fatal("field-selected replay changed identity or sent again")
			}
		})
	}
}

func TestResolveIdentity_FieldSelectedUserAndChannel_PreservesCanonicalIdentity(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		t.Run(kind, func(t *testing.T) {
			_, s, value, _ := identityQueryFixture(t, kind)
			trimmed := honorIdentityFieldSelection(t, s)
			id, err := s.graph.resolveIdentity(context.Background(), kind, value, 20)
			if *trimmed == 0 {
				t.Fatal("field selection was not exercised")
			}
			if err != nil || id <= 0 {
				t.Fatalf("field-selected %s identity failed: %v", kind, err)
			}
			reused, err := s.graph.resolveIdentity(context.Background(), kind, value, 20)
			if err != nil || reused != id {
				t.Fatalf("field-selected %s identity changed: %v", kind, err)
			}
		})
	}
}
