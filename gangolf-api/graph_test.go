package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestGraph_PrivateAccessWireMetadata_EnforcesExplicitPrivateValues(t *testing.T) {
	cases := []struct {
		name, field string
		private     bool
	}{
		{"enum-none", `,"access":"None"`, true},
		{"numeric-zero", `,"access":0`, true},
		{"escaped-enum-none", `,"access":"\u004eone"`, true},
		{"missing", "", false},
		{"null", `,"access":null`, false},
		{"read", `,"access":"Read"`, false},
		{"write", `,"access":"Write"`, false},
		{"read-write", `,"access":"Read, Write"`, false},
		{"numeric-read", `,"access":1`, false},
		{"numeric-write", `,"access":2`, false},
		{"numeric-read-write", `,"access":3`, false},
		{"unknown-name", `,"access":"Private"`, false},
		{"empty-name", `,"access":""`, false},
		{"wrong-case", `,"access":"none"`, false},
		{"string-zero", `,"access":"0"`, false},
		{"negative", `,"access":-1`, false},
		{"unknown-bits", `,"access":4`, false},
		{"fraction", `,"access":0.5`, false},
		{"boolean", `,"access":false`, false},
		{"object", `,"access":{}`, false},
		{"array", `,"access":[]`, false},
	}
	for _, tc := range cases {
		for _, operation := range []string{"config", "append", "list"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				f, s := newFixture(t)
				var m message
				if err := json.Unmarshal([]byte(testMessage), &m); err != nil {
					t.Fatal(err)
				}
				m.Time = s.now()
				payload, err := json.Marshal(batch{SchemaVersion: 1, Messages: []message{m}})
				if err != nil {
					t.Fatal(err)
				}
				content, err := json.Marshal(string(payload))
				if err != nil {
					t.Fatal(err)
				}
				metadataReads, contentReads, creates, links, uploads := 0, 0, 0, 0, 0
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.URL.Path == "/users/me":
						respond(w, 200, map[string]any{"id": 20, "enabled": true, "permissions": []string{"write"}})
					case r.URL.Path == "/nodes/10" || r.URL.Path == "/nodes/11":
						id := int64(10)
						if r.URL.Path == "/nodes/11" {
							id = 11
						}
						respond(w, 200, node{ID: id, Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)})
					case r.Method == "GET" && r.URL.Path == "/nodes/12":
						metadataReads++
						fmt.Fprintf(w, `{"id":12,"rootNodeId":10,"ownerId":20%s}`, tc.field)
					case r.Method == "GET" && r.URL.Path == "/nodes/12/content":
						contentReads++
						respond(w, 200, f.cfg)
					case r.Method == "POST" && r.URL.Path == "/nodes":
						creates++
						fmt.Fprintf(w, `{"id":100,"type":"gangolf-message-batch","rootNodeId":10%s}`, tc.field)
					case r.Method == "POST" && r.URL.Path == "/nodes/100/links":
						links++
					case r.Method == "POST" && r.URL.Path == "/nodes/100/content":
						uploads++
					case r.Method == "GET" && r.URL.Path == "/nodes":
						metadataReads++
						fmt.Fprintf(w, `{"result":[{"id":100,"type":"gangolf-message-batch","rootNodeId":10%s,"contentType":"application/json","content":%s}],"total":1}`, tc.field, content)
					default:
						t.Errorf("unexpected graph request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer host.Close()
				s.graph.base, err = url.Parse(host.URL)
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "config":
					var cfg *authConfig
					cfg, err = s.graph.loadConfig(context.Background())
					if metadataReads != 1 || tc.private && (cfg == nil || contentReads != 1) || !tc.private && (cfg != nil || contentReads != 0) {
						t.Fatal("config access gate did not control content loading")
					}
				case "append":
					var id int64
					id, err = s.graph.append(context.Background(), []message{m})
					if creates != 1 || tc.private && (id != 100 || links != 1 || uploads != 1) || !tc.private && (id != 0 || links != 0 || uploads != 0) {
						t.Fatal("create access gate did not control publication")
					}
				case "list":
					var messages []storedMessage
					messages, err = s.graph.list(context.Background())
					if metadataReads != 1 || tc.private && (len(messages) != 1 || messages[0].User.Name != "Distinct Speaker") || !tc.private && len(messages) != 0 {
						t.Fatal("list access gate did not control message disclosure")
					}
				}
				if tc.private && err != nil || !tc.private && err != errCorrupt {
					t.Fatalf("private=%v error=%v", tc.private, err)
				}
			})
		}
	}
}
