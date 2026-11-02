package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTwitchCredentialDirectoryRejectsTraversalSymlinkHardlinkAndPublicFiles(t *testing.T) {
	for _, kind := range []string{"valid", "traversal", "absolute", "file-symlink", "directory-symlink", "hardlink", "public-file", "public-directory", "malformed", "oversize", "missing", "empty-access", "missing-refresh", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			f := newTwitchFake(t)
			directory, ref := f.client.directory, "test-grant"
			path := filepath.Join(directory, ref)
			switch kind {
			case "traversal":
				ref = "../test-grant"
			case "absolute":
				ref = path
			case "file-symlink":
				if err := os.Symlink("test-grant", filepath.Join(directory, "symlink")); err != nil {
					t.Fatal(err)
				}
				ref = "symlink"
			case "directory-symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(directory, link); err != nil {
					t.Fatal(err)
				}
				directory = link
			case "hardlink":
				if err := os.Link(path, filepath.Join(directory, "hardlink")); err != nil {
					t.Fatal(err)
				}
				ref = "hardlink"
			case "public-file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(directory, 0755); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte(`{"accessToken":"local","refreshToken":"local","extra":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, []byte(strings.Repeat("x", 16385)), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				ref = "missing"
			case "empty-access":
				if err := os.WriteFile(path, []byte(`{"accessToken":"","refreshToken":"local"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-refresh":
				if err := os.WriteFile(path, []byte(`{"accessToken":"local"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				if os.Geteuid() != 0 {
					t.Skip("foreign ownership requires root")
				}
				if err := os.Chown(path, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			}
			credential, err := readTwitchCredential(directory, ref)
			if kind == "valid" {
				if err != nil || credential.AccessToken != "local-access" {
					t.Fatal("valid credential rejected")
				}
			} else if err != errTwitchCredential {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
}

func TestTwitchTokenClientSubjectScopeValidationAndRefreshRotation(t *testing.T) {
	for _, kind := range []string{"valid", "client", "subject", "scope", "expired", "refresh-rejected", "rotation-failed", "revalidation-failed", "malformed-refresh", "transport"} {
		t.Run(kind, func(t *testing.T) {
			_, _, p, _ := inboundFixture(t)
			f := newTwitchFake(t)
			f.validate = func(r *http.Request) (int, any) {
				if !strings.HasPrefix(r.Header.Get("Authorization"), "OAuth ") {
					t.Error("missing validation authorization")
				}
				body := map[string]any{"client_id": p.ClientID, "user_id": p.BotUserID, "scopes": []string{"user:read:chat"}, "expires_in": 3600}
				switch kind {
				case "client":
					body["client_id"] = "other"
				case "subject":
					body["user_id"] = "901"
				case "scope":
					body["scopes"] = []string{"chat:read"}
				case "expired", "refresh-rejected", "rotation-failed", "malformed-refresh":
					if r.Header.Get("Authorization") == "OAuth local-access" {
						return 401, map[string]any{}
					}
				case "revalidation-failed":
					return 401, map[string]any{}
				case "transport":
					return 503, map[string]any{}
				}
				return 200, body
			}
			f.refresh = func(r *http.Request) (int, any) {
				if r.ParseForm() != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "local-refresh" || r.Form.Get("client_id") != p.ClientID || r.Form.Get("client_secret") != "local-client" || r.URL.RawQuery != "" {
					t.Error("invalid refresh request")
				}
				if kind == "refresh-rejected" {
					return 400, map[string]any{}
				}
				if kind == "rotation-failed" {
					if err := os.Chmod(f.client.directory, 0755); err != nil {
						t.Error(err)
					}
				}
				if kind == "malformed-refresh" {
					return 200, map[string]any{"access_token": "new"}
				}
				return 200, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh"}
			}
			credential, err := f.client.authorize(context.Background(), p)
			if kind == "valid" || kind == "expired" {
				if err != nil {
					t.Fatal(err)
				}
				if kind == "expired" {
					if credential.AccessToken != "rotated-access" {
						t.Fatal("rotation not used")
					}
					stored, err := readTwitchCredential(f.client.directory, p.CredentialRef)
					if err != nil || stored != credential {
						t.Fatal("rotation not durable")
					}
					entries, err := os.ReadDir(f.client.directory)
					if err != nil || len(entries) != 1 {
						t.Fatal("temporary credential residue")
					}
				}
			} else if err == nil {
				t.Fatal("invalid credential accepted")
			}
			f.mu.Lock()
			refreshes := f.refreshCalls
			f.mu.Unlock()
			wantRefresh := kind == "expired" || kind == "refresh-rejected" || kind == "rotation-failed" || kind == "malformed-refresh" || kind == "revalidation-failed"
			if (refreshes == 1) != wantRefresh {
				t.Fatalf("refresh count=%d", refreshes)
			}
		})
	}
}

func TestTwitchSubscriptionValidatesWelcomeSessionConditionsAndRefreshesOnce(t *testing.T) {
	for _, kind := range []string{"valid", "id", "status", "type", "version", "broadcaster", "subject", "session", "method", "empty-data", "unauthorized", "forbidden", "rate-limit", "server-error", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			_, _, p, _ := inboundFixture(t)
			f := newTwitchFake(t)
			attempts := 0
			f.subscribe = func(r *http.Request, input twitchSubscription) (int, any) {
				attempts++
				sub := fakeSubscription(input.Transport.SessionID)
				switch kind {
				case "id":
					sub.ID = ""
				case "status":
					sub.Status = "pending"
				case "type":
					sub.Type = "other"
				case "version":
					sub.Version = "2"
				case "broadcaster":
					sub.Condition.BroadcasterUserID = "999"
				case "subject":
					sub.Condition.UserID = "999"
				case "session":
					sub.Transport.SessionID = "other"
				case "method":
					sub.Transport.Method = "webhook"
				case "empty-data":
					return 202, map[string]any{"data": []any{}}
				case "unauthorized":
					if attempts == 1 {
						return 401, map[string]any{}
					}
				case "forbidden":
					return 403, map[string]any{}
				case "rate-limit":
					if attempts == 1 {
						return 429, map[string]any{}
					}
				case "server-error":
					if attempts == 1 {
						return 503, map[string]any{}
					}
				case "redirect":
					return 302, map[string]any{}
				}
				return 202, map[string]any{"data": []twitchSubscription{sub}}
			}
			credential, err := readTwitchCredential(f.client.directory, p.CredentialRef)
			if err != nil {
				t.Fatal(err)
			}
			id, err := f.client.subscribe(context.Background(), p, "session-1", &credential)
			valid := kind == "valid" || kind == "unauthorized" || kind == "rate-limit" || kind == "server-error"
			if valid && (err != nil || id != "subscription-1") || !valid && err == nil {
				t.Fatalf("kind=%s id=%s err=%v", kind, id, err)
			}
			if kind == "unauthorized" && credential.AccessToken != "rotated-access" {
				t.Fatal("refresh not adopted")
			}
			if (kind == "unauthorized" || kind == "rate-limit" || kind == "server-error") && attempts != 2 {
				t.Fatalf("attempts=%d", attempts)
			}
		})
	}
}

func TestTwitchCredentialWireContainsNoUnneededFields(t *testing.T) {
	f := newTwitchFake(t)
	c, err := readTwitchCredential(f.client.directory, "test-grant")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	if len(fields) != 3 {
		t.Fatal("unexpected credential schema")
	}
}

func TestTwitchRetryAfterAndRateResetAreHonored(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		headers http.Header
		want    time.Duration
	}{
		{http.Header{"Retry-After": []string{"120"}}, 120 * time.Second},
		{http.Header{"Retry-After": []string{now.Add(2 * time.Minute).Format(http.TimeFormat)}}, 2 * time.Minute},
		{http.Header{"Ratelimit-Reset": []string{strconv.FormatInt(now.Add(3*time.Minute).Unix(), 10)}}, 3 * time.Minute},
		{http.Header{}, time.Second},
	} {
		if got := retryDelay(test.headers, now); got != test.want {
			t.Fatalf("got=%s want=%s", got, test.want)
		}
	}
}
