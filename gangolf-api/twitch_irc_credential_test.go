package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyIRCCredentialAndValidation(t *testing.T) {
	for _, input := range []string{"User=TestBot\nToken=local-access\n", "\n User = TestBot \r\n Token=oauth:local-access \r\n", "User=testbot\nToken=a=b=c"} {
		c, err := parseLegacyCredential([]byte(input))
		if err != nil || c.Login != "testbot" || c.AccessToken == "" || c.RefreshToken != "" {
			t.Fatal("valid credential rejected")
		}
		if strings.HasSuffix(input, "a=b=c") && c.AccessToken != "a=b=c" {
			t.Fatal("first equals not preserved")
		}
	}
	for _, input := range []string{"User=testbot", "Token=sentinel", "User=\nToken=sentinel", "User=testbot\nToken=oauth:", "User=testbot\nToken=sentinel\nToken=other", "User=testbot\nUser=other\nToken=sentinel", "User=testbot\nToken=sentinel\nUnknown=value", "User=testbot\nToken=sentinel\x00", "User=testbot\nToken=sentinel\rBAD", "User=testbot\nToken=sentinel\t", "User=bad user\nToken=sentinel", "User=testbot\nToken=oauth: bad"} {
		if _, err := parseLegacyCredential([]byte(input)); err == nil || strings.Contains(err.Error(), "sentinel") {
			t.Fatal("unsafe credential accepted or leaked")
		}
	}
	for _, mode := range []string{"read-only", "empty-scopes", "wrong-login", "wrong-bot", "wrong-client", "unknown-expiry", "missing-read", "invalid", "malformed-identity"} {
		t.Run(mode, func(t *testing.T) {
			_, s, tw, p := ircProfileFixture(t)
			tw.validate = func(*http.Request) (int, any) {
				login, bot, client, expires := "testbot", "900", "testclient", 3600
				scopes := []string{"chat:read", "chat:edit"}
				switch mode {
				case "read-only":
					scopes = []string{"chat:read"}
				case "wrong-login":
					login = "other"
				case "wrong-bot":
					bot = "901"
				case "wrong-client":
					client = "other"
				case "unknown-expiry":
					expires = 0
				case "missing-read":
					scopes = []string{"chat:edit"}
				case "empty-scopes":
					scopes = nil
				case "malformed-identity":
					login = ""
				case "invalid":
					return 401, map[string]any{}
				}
				return 200, map[string]any{"login": login, "user_id": bot, "client_id": client, "expires_in": expires, "scopes": scopes}
			}
			credential, receiveErr := s.twitch.client.authorize(context.Background(), p)
			_, sendErr := s.twitch.client.authorizeSend(context.Background(), p)
			if mode == "read-only" || mode == "empty-scopes" || mode == "unknown-expiry" || mode == "missing-read" {
				if receiveErr != nil || sendErr != nil || credential.diagnostic != "irc_metadata_unconfirmed" || credential.revalidateAt.IsZero() {
					t.Fatal("IRC metadata hard rejection or missing diagnostics")
				}
			} else if receiveErr == nil || sendErr == nil {
				t.Fatal("validation accepted mismatch")
			}
			if tw.validationCalls != 2 || tw.refreshCalls != 0 {
				t.Fatal("validation not reached or refresh attempted")
			}
		})
	}
	_, _, tw, p := ircProfileFixture(t)
	path := filepath.Join(tw.client.directory, "test-grant")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProfileCredential(tw.client.directory, p); err == nil {
		t.Fatal("public credential accepted")
	}
}

func TestTwitchProfileTransportIsolation(t *testing.T) {
	for _, mode := range []string{"valid", "missing-transport", "missing-login", "uppercase-login", "invalid-login", "wrong-schema", "eventsub-transport"} {
		t.Run(mode, func(t *testing.T) {
			f, s, _, _ := ircProfileFixture(t)
			changeProfile(f, 30, func(p *twitchProfile) {
				switch mode {
				case "missing-transport":
					p.Transport = ""
				case "missing-login":
					p.ChannelLogin = ""
				case "uppercase-login":
					p.ChannelLogin = "Rugenforth"
				case "invalid-login":
					p.ChannelLogin = "bad\r\n"
				case "wrong-schema":
					p.SchemaVersion = 3
				case "eventsub-transport":
					p.SchemaVersion = 1
				}
			})
			_, err := s.graph.loadConfig(context.Background())
			if (err == nil) != (mode == "valid") {
				t.Fatalf("transport mode=%s err=%v", mode, err)
			}
		})
	}
	f, s, _, _ := ircProfileFixture(t)
	provisionTwitch(t, f, 40, "7", "101")
	changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true); p.ChannelID = "31" })
	delete(f.nodes, 41)
	filtered := f.edges[:0]
	for _, edge := range f.edges {
		if edge.Source != 41 {
			filtered = append(filtered, edge)
		}
	}
	f.edges = filtered
	for i := range f.edges {
		if f.edges[i].Source == 40 && f.edges[i].Context == "configured-for" {
			f.edges[i].Target = 31
		}
	}
	if _, err := s.graph.validateTwitchProfile(context.Background(), f.projection(f.nodes[40]), 20); err != nil {
		t.Fatalf("second profile not valid: %v", err)
	}
	if _, err := s.graph.loadConfig(context.Background()); err == nil {
		t.Fatal("duplicate broadcaster modes accepted")
	}
}
