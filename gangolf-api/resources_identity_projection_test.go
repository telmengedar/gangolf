package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestResolveIdentity_EquivalentProjections_PreservesIdentityAndRejectsCorruption(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, phase := range []string{"I", "item"} {
			for _, change := range []string{"unchanged", "numeric-private", "shuffled-links", "numeric-and-shuffled", "owner", "root", "type", "name", "content", "content-type", "public-access", "missing-access", "string-zero-access", "edge-source", "edge-target", "edge-type", "edge-context", "extra-edge", "duplicate-edge"} {
				t.Run(kind+"/"+phase+"/"+change, func(t *testing.T) {
					f, s := newResourceFixture(t)
					original := createResource(t, s, v3Message)
					id := original.UserID
					if kind == "channel" {
						id = original.ChannelID
					}
					numericID, _ := resourceID(id)
					if len(f.projection(f.nodes[numericID]).Links) < 2 {
						t.Fatal("published identity must have membership and message links")
					}
					exercised := 0
					observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
						q := r.URL.Query()
						match := namedIdentityRequest(r, kind) && q.Get("linkedto") == ""
						if phase == "item" {
							match = r.Method == "GET" && r.URL.Path == "/api/nodes" && q.Get("id") == id
						}
						if !match || exercised != 0 {
							return
						}
						exercised++
						rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
							var rows []node
							if err := json.Unmarshal(page["result"], &rows); err != nil || len(rows) != 1 || rows[0].ID != numericID {
								t.Fatalf("identity projection rows=%v err=%v", rows, err)
							}
							n := &rows[0]
							if change != "unchanged" {
								n.Access = json.RawMessage("0")
							}
							if change != "unchanged" && change != "numeric-private" {
								slices.Reverse(n.Links)
							}
							if change == "shuffled-links" {
								n.Access = json.RawMessage(`"None"`)
							}
							switch change {
							case "owner":
								n.Owner++
							case "root":
								n.Root++
							case "type":
								n.Type = "message"
							case "name":
								n.Name += "other"
							case "content":
								content := "{}"
								n.Content = &content
							case "content-type":
								n.ContentType = "text/plain"
							case "public-access":
								n.Access = json.RawMessage(`"Read"`)
							case "missing-access":
								n.Access = nil
							case "string-zero-access":
								n.Access = json.RawMessage(`"0"`)
							case "edge-source":
								n.Links[0].Source++
							case "edge-target":
								n.Links[0].Target++
							case "edge-type":
								n.Links[0].Type = "Bidirectional"
							case "edge-context":
								n.Links[0].Context = "unexpected"
							case "extra-edge":
								extra := n.Links[0]
								extra.Source = 900
								n.Links = append(n.Links, extra)
							case "duplicate-edge":
								n.Links = append(n.Links, n.Links[0])
							}
							page["result"], _ = json.Marshal(rows)
						})
					})
					beforeWrites, beforeNodes := f.writes, len(f.nodes)
					beforeEdges := slices.Clone(f.edges)
					w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
					if exercised != 1 {
						t.Fatalf("projection phase exercised %d times", exercised)
					}
					valid := change == "unchanged" || change == "numeric-private" || change == "shuffled-links" || change == "numeric-and-shuffled"
					if !valid {
						if w.Code != 500 || !strings.Contains(w.Body.String(), `"corrupt_storage"`) || f.writes != beforeWrites || len(f.nodes) != beforeNodes || !reflect.DeepEqual(f.edges, beforeEdges) {
							t.Fatalf("corrupt projection status=%d body=%s writes=%d nodes=%d", w.Code, w.Body.String(), f.writes-beforeWrites, len(f.nodes)-beforeNodes)
						}
						return
					}
					var created messageResource
					if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.UserID != original.UserID || created.ChannelID != original.ChannelID || len(f.nodes) != beforeNodes+1 || f.writes <= beforeWrites {
						t.Fatalf("equivalent projection status=%d body=%s writes=%d nodes=%d original=%d", w.Code, w.Body.String(), f.writes-beforeWrites, len(f.nodes)-beforeNodes, numericID)
					}
				})
			}
		}
	}
}
