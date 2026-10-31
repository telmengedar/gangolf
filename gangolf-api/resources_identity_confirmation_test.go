package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestIdentityQueries_ConfirmationPreservesOriginalIDBeforePublication(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, state := range []string{"create", "sterile-resume"} {
			for _, confirmation := range []string{"different-valid-id", "same-id"} {
				t.Run(kind+"/"+state+"/"+confirmation, func(t *testing.T) {
					f, s, v, group := identityQueryFixture(t, kind)
					originalID := f.next
					if state == "sterile-resume" {
						originalID = seedQueryIdentity(f, kind, v, false, false).ID
					}
					key := v.ServiceID
					if kind == "channel" {
						key = v.Channel
					}
					original := node{ID: originalID, Name: identityName(kind, v.Service, key), Type: kind, Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)}
					canonical, publications, itemConfirmations := 0, 0, 0
					var returned node
					observeIdentityWire(s, func(r *http.Request) {
						if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/content") {
							publications++
							if canonical != 2 || itemConfirmations != 1 {
								t.Error("publication preceded canonical and item confirmation")
							}
						}
					}, func(r *http.Request, response *http.Response) {
						if namedIdentityRequest(r, kind) && r.URL.Query().Get("linkedto") != "" {
							canonical++
							if canonical != 2 {
								return
							}
							rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
								var rows []node
								if err := json.Unmarshal(page["result"], &rows); err != nil || len(rows) != 1 || rows[0].ID != originalID {
									t.Fatalf("original confirmation rows=%v err=%v", rows, err)
								}
								returned = rows[0]
								if confirmation == "different-valid-id" {
									returned.ID = 91
									returned.Links = []relation{{Source: 91, Target: group, Type: "None"}}
								}
								if returned.Name != original.Name || returned.Type != kind || returned.Root != 10 || returned.Owner != 20 || !returned.privateAccess() || !exactMembership(returned, group) || returned.Content != nil {
									t.Fatalf("invalid confirmation fixture: %+v", returned)
								}
								page["result"], _ = json.Marshal([]node{returned})
							})
						} else if r.Method == http.MethodGet && r.URL.Path == "/api/nodes" && returned.ID != 0 && r.URL.Query().Get("id") == strconv.FormatInt(returned.ID, 10) {
							itemConfirmations++
							rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
								page["result"], _ = json.Marshal([]node{returned})
								page["total"] = json.RawMessage("1")
							})
						}
					})
					id, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
					if canonical != 2 || returned.ID == 0 {
						t.Fatalf("confirmation not exercised: canonical=%d returned=%+v", canonical, returned)
					}
					stored, exists := f.nodes[originalID]
					if !exists || len(f.nodes) != 1 || !exactMembership(f.projection(stored), group) {
						t.Fatalf("original identity replaced: exists=%v nodes=%v", exists, f.nodes)
					}
					if confirmation == "different-valid-id" {
						if !errors.Is(err, errCorrupt) || id != 0 || publications != 0 || itemConfirmations != 0 || !reflect.DeepEqual(stored, original) {
							t.Fatalf("different ID accepted: id=%d err=%v publicationAttempts=%d itemConfirmations=%d stored=%+v", id, err, publications, itemConfirmations, stored)
						}
						return
					}
					if err != nil || id != originalID || publications != 1 || itemConfirmations != 1 {
						t.Fatalf("same ID rejected: id=%d err=%v publicationAttempts=%d itemConfirmations=%d", id, err, publications, itemConfirmations)
					}
					if _, err := identityValue(stored, kind, v.Service, key); err != nil {
						t.Fatalf("same-ID content not published: %v", err)
					}
				})
			}
		}
	}
}
