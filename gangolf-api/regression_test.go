package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTUserIDMapsStringAndIntegerClaimsToStoredGrants(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		id    any
		grant grant
		want  int
	}{
		{"string", "7", grant{Enabled: true, Permissions: []string{"admin"}}, 201},
		{"integer", 7, grant{Enabled: true, Permissions: []string{"host"}}, 201},
		{"unknown-integer", 8, grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"disabled-integer", 7, grant{Enabled: false, Permissions: []string{"host"}}, 403},
		{"unprivileged-integer", 7, grant{Enabled: true}, 403},
		{"fraction", 7.5, grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"decimal", json.Number("7.0"), grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"exponent", json.Number("7e0"), grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"overflow", json.Number("9223372036854775808"), grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"boolean", true, grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"null", nil, grant{Enabled: true, Permissions: []string{"host"}}, 403},
		{"array", []int{7}, grant{Enabled: true, Permissions: []string{"host"}}, 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, s := newFixture(t)
			f.cfg.Users["7"] = tt.grant
			f.jwks = []byte(fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"test-key","alg":"RS256","use":"sig","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(private.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes())))
			claims := jwt.MapClaims{"iss": f.cfg.Issuer, "aud": f.cfg.Audience, "exp": s.now().Add(time.Hour).Unix(), "userId": tt.id, "permission": "admin"}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test-key"
			encoded, err := token.SignedString(private)
			if err != nil {
				t.Fatal(err)
			}
			status(t, request(t, s, "POST", "/api/v2/messages", encoded, "["+testMessage+"]"), tt.want)
			if tt.want == 201 {
				if len(f.nodes) != 1 || f.nodes[0].Content == nil || f.uploads != 1 {
					t.Fatal("authorized claim did not publish a batch")
				}
			} else if f.writes != 0 || len(f.nodes) != 0 {
				t.Fatal("unusable identity reached storage mutation")
			}
		})
	}
}

func TestMalformedListQueryRejectsInsteadOfReturningStoredMessages(t *testing.T) {
	for _, query := range []string{
		"from=%ZZ", "count=%ZZ", "service=%ZZ", "channel=%ZZ",
		"service=localhost;channel=private", "channel=host;service=other",
		"service=other&count=%ZZ", "service=localhost&channel=%ZZ",
	} {
		for _, queryKey := range []bool{false, true} {
			t.Run(query+"/query-key="+strconv.FormatBool(queryKey), func(t *testing.T) {
				f, s := newFixture(t)
				status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]"), 201)
				control := request(t, s, "GET", "/api/v2/messages?service=localhost&channel=host", "key.secret", "")
				status(t, control, 200)
				if !strings.Contains(control.Body.String(), "Distinct Speaker") {
					t.Fatal("control did not return stored message")
				}
				token, rawQuery := "key.secret", query
				if queryKey {
					token = ""
					rawQuery += "&apiKey=key.secret"
				}
				w := request(t, s, "GET", "/api/v2/messages?"+rawQuery, token, "")
				status(t, w, 400)
				var failure map[string]string
				if json.Unmarshal(w.Body.Bytes(), &failure) != nil || failure["code"] != "invalid_filter" || strings.Contains(w.Body.String(), "Distinct Speaker") {
					t.Fatalf("malformed query did not return sanitized filter error: %s", w.Body.String())
				}
				if f.writes != 3 || f.uploads != 1 {
					t.Fatal("list changed stored batch")
				}
			})
		}
	}
}

func TestMultiMessageBatchPreservesAllMessagesAndWithinBatchOrder(t *testing.T) {
	f, s := newFixture(t)
	names := []string{"alpha-first", "beta-middle", "gamma-last"}
	bodies := make([]string, len(names))
	for i, name := range names {
		bodies[i] = strings.Replace(testMessage, "Distinct Speaker", name, 1)
	}
	w := request(t, s, "POST", "/api/v2/messages", "key.secret", "["+strings.Join(bodies, ",")+"]")
	status(t, w, 201)
	var accepted struct{ Result []message }
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if len(accepted.Result) != len(names) || len(f.nodes) != 1 || f.uploads != 1 || f.nodes[0].Content == nil {
		t.Fatalf("batch was not accepted together: %s", w.Body.String())
	}
	for i, m := range accepted.Result {
		if m.User.Name != names[i] || !m.Time.Equal(s.now()) {
			t.Fatalf("accepted message %d changed: %+v", i, m)
		}
	}
	restarted := &server{graph: s.graph, pepper: s.pepper, now: s.now}
	for _, descending := range []bool{false, true} {
		t.Run("descending="+strconv.FormatBool(descending), func(t *testing.T) {
			var got []message
			for offset := 0; offset < len(names); offset += 2 {
				w := request(t, restarted, "GET", "/api/v2/messages?count=2&continue="+strconv.Itoa(offset)+"&descending="+strconv.FormatBool(descending), "key.secret", "")
				status(t, w, 200)
				var page struct {
					Result   []message
					Total    int
					Continue *int
				}
				if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if page.Total != len(names) || len(page.Result) != min(2, len(names)-offset) {
					t.Fatalf("batch lost messages: %s", w.Body.String())
				}
				if offset == 0 {
					if page.Continue == nil || *page.Continue != 2 {
						t.Fatalf("missing continuation: %s", w.Body.String())
					}
				} else if page.Continue != nil {
					t.Fatalf("unexpected continuation: %s", w.Body.String())
				}
				got = append(got, page.Result...)
			}
			for i, m := range got {
				index := i
				if descending {
					index = len(names) - 1 - i
				}
				want, err := json.Marshal(accepted.Result[index])
				if err != nil {
					t.Fatal(err)
				}
				actual, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if string(actual) != string(want) {
					t.Fatalf("message %d changed or reordered: got %s want %s", i, actual, want)
				}
			}
		})
	}
}
