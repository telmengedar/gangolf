package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func operatorKeyFixture(t *testing.T) (*resourceFixture, *server, string, string) {
	t.Helper()
	f, s := newResourceFixture(t)
	user := f.legacy.cfg.Users["7"]
	user.Permissions = []string{"host"}
	f.legacy.cfg.Users["7"] = user
	key := f.legacy.cfg.APIKeys["key"]
	key.Permissions = []string{"host"}
	f.legacy.cfg.APIKeys["key"] = key
	output := filepath.Join(t.TempDir(), "bootstrap.key")
	id, _, err := s.bootstrapAdmin(context.Background(), "Administrator", output)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return f, s, id, strings.TrimSpace(string(raw))
}

func savedOperatorKey(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !safeBootstrapOutput(info, info) {
		t.Fatal("unsafe operator output")
	}
	return strings.TrimSpace(string(raw))
}

func TestOperatorAdminKeyRenewalBeyondOriginalCutoff(t *testing.T) {
	for _, days := range []int{10, 31} {
		name := "before-expiry"
		if days > 30 {
			name = "after-expiry"
		}
		t.Run(name, func(t *testing.T) {
			f, s, userID, oldToken := operatorKeyFixture(t)
			originalNow := s.now()
			originalExpiry := originalNow.Add(30 * 24 * time.Hour)
			requested := originalExpiry.Add(time.Hour).Format(time.RFC3339Nano)
			status(t, appRequest(t, s, "POST", "/api/v3/users/"+userID+"/tokens", oldToken, `{"permissions":["admin","host"],"expiresAt":"`+requested+`"}`), 400)
			later := originalNow.Add(time.Duration(days) * 24 * time.Hour)
			s.now = func() time.Time { return later }
			if days > 30 {
				status(t, appRequest(t, s, "GET", "/api/v3/users/current", oldToken, ""), 401)
			}
			before, _ := json.Marshal(f.legacy.cfg)
			profileCount := len(f.nodes)
			writes := f.writes
			output := filepath.Join(t.TempDir(), "renewed.key")
			result, err := s.operatorAdminKey(context.Background(), userID, "", output, nil)
			if err != nil || result.UserID != userID || !result.ExpiresAt.Equal(later.Add(30*24*time.Hour)) || !result.ExpiresAt.After(originalExpiry) {
				t.Fatalf("renewal cutoff did not advance: %v", err)
			}
			token := savedOperatorKey(t, output)
			status(t, appRequest(t, s, "GET", "/api/v3/users", token, ""), 200)
			key := f.legacy.cfg.APIKeys[result.KeyID]
			if key.Scope != "api" || !reflect.DeepEqual(key.Permissions, []string{"admin", "host"}) || len(key.ChannelIDs) != 0 || key.ExpiresAt == nil {
				t.Fatal("operator key did not retain fixed finite scope")
			}
			after, _ := json.Marshal(f.legacy.cfg)
			var restored authConfig
			json.Unmarshal(after, &restored)
			delete(restored.APIKeys, result.KeyID)
			preserved, _ := json.Marshal(restored)
			if string(preserved) != string(before) || len(f.nodes) != profileCount || f.writes-writes != 1 {
				t.Fatal("renewal changed existing config/profile or wrote more than one auth replacement")
			}
			tooLate := result.ExpiresAt.Add(time.Hour).Format(time.RFC3339Nano)
			status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", token, `{"expiresAt":"`+tooLate+`"}`), 400)
			beyondCutoff := originalExpiry.Add(24 * time.Hour)
			if later.After(beyondCutoff) {
				beyondCutoff = later
			}
			s.now = func() time.Time { return beyondCutoff }
			status(t, appRequest(t, s, "GET", "/api/v3/users/current", oldToken, ""), 401)
			status(t, appRequest(t, s, "GET", "/api/v3/users", token, ""), 200)
		})
	}
}

func TestOperatorAdminKeyTargetBoundary(t *testing.T) {
	for _, mutation := range []string{"missing", "host-only", "disabled", "deleted", "missing-profile", "wrong-binding", "wrong-root", "public-profile", "corrupt-config"} {
		t.Run(mutation, func(t *testing.T) {
			f, s, userID, _ := operatorKeyFixture(t)
			user := f.legacy.cfg.Users[userID]
			profileID := mustResourceID(user.ProfileNodeID)
			switch mutation {
			case "missing":
				userID = "999"
			case "host-only":
				user.Permissions = []string{"host"}
				f.legacy.cfg.Users[userID] = user
			case "disabled":
				user.Enabled = false
				f.legacy.cfg.Users[userID] = user
			case "missing-profile":
				user.ProfileNodeID = ""
				f.legacy.cfg.Users[userID] = user
			case "wrong-binding":
				n := f.nodes[profileID]
				var profile appUser
				json.Unmarshal([]byte(*n.Content), &profile)
				profile.UserID = "999"
				raw, _ := json.Marshal(profile)
				n.Content = new(string(raw))
				f.nodes[profileID] = n
			case "deleted":
				n := f.nodes[profileID]
				var profile appUser
				json.Unmarshal([]byte(*n.Content), &profile)
				profile.Deleted = true
				raw, _ := json.Marshal(profile)
				n.Content = new(string(raw))
				f.nodes[profileID] = n
			case "wrong-root":
				n := f.nodes[profileID]
				n.Root = 999
				f.nodes[profileID] = n
			case "public-profile":
				n := f.nodes[profileID]
				n.Access = json.RawMessage("2")
				f.nodes[profileID] = n
			case "corrupt-config":
				f.legacy.corruptConfig = true
			}
			before, _ := json.Marshal(f.legacy.cfg)
			writes, nodes := f.writes, len(f.nodes)
			_, err := s.operatorAdminKey(context.Background(), userID, "", filepath.Join(t.TempDir(), "new.key"), nil)
			if err == nil || f.writes != writes || len(f.nodes) != nodes {
				t.Fatal("invalid target mutated security/profile")
			}
			after, _ := json.Marshal(f.legacy.cfg)
			if string(before) != string(after) {
				t.Fatal("invalid target changed grants or keys")
			}
		})
	}
}

func TestOperatorAdminKeyExpiry(t *testing.T) {
	for _, mode := range []string{"default", "offset", "maximum", "past", "now", "too-late"} {
		t.Run(mode, func(t *testing.T) {
			f, s, userID, _ := operatorKeyFixture(t)
			now := s.now().UTC()
			var expiry *time.Time
			switch mode {
			case "offset":
				value := now.Add(48 * time.Hour).In(time.FixedZone("offset", 3600))
				expiry = &value
			case "maximum":
				value := now.Add(365 * 24 * time.Hour)
				expiry = &value
			case "past":
				value := now.Add(-time.Second)
				expiry = &value
			case "now":
				expiry = &now
			case "too-late":
				value := now.Add(365*24*time.Hour + time.Nanosecond)
				expiry = &value
			}
			before := f.writes
			result, err := s.operatorAdminKey(context.Background(), userID, "", filepath.Join(t.TempDir(), "expiry.key"), expiry)
			if mode == "past" || mode == "now" || mode == "too-late" {
				if err == nil || f.writes != before {
					t.Fatal("invalid expiry activated key")
				}
				return
			}
			if err != nil || result.ExpiresAt.Location() != time.UTC || f.legacy.cfg.APIKeys[result.KeyID].ExpiresAt == nil {
				t.Fatal("finite UTC expiry missing")
			}
			expected := now.Add(30 * 24 * time.Hour)
			if expiry != nil {
				expected = expiry.UTC()
			}
			if !result.ExpiresAt.Equal(expected) {
				t.Fatal("expiry changed")
			}
		})
	}
}

func TestOperatorAdminKeyOutputCustody(t *testing.T) {
	for _, mutation := range []string{"existing", "symlink", "unlink", "replace", "tamper", "public", "hardlink", "after-submit"} {
		t.Run(mutation, func(t *testing.T) {
			f, s, userID, _ := operatorKeyFixture(t)
			directory := t.TempDir()
			output := filepath.Join(directory, "admin.key")
			if mutation == "existing" {
				if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mutation == "symlink" {
				if err := os.Symlink(filepath.Join(directory, "target"), output); err != nil {
					t.Fatal(err)
				}
			}
			changed := false
			observeIdentityWire(s, func(r *http.Request) {
				if changed || mutation == "existing" || mutation == "symlink" {
					return
				}
				if (mutation == "after-submit" || mutation == "tamper") && !(r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/12/content")) {
					return
				}
				changed = true
				var err error
				switch mutation {
				case "unlink", "after-submit":
					err = os.Remove(output)
				case "replace":
					err = os.Remove(output)
					if err == nil {
						err = os.WriteFile(output, []byte("replaced"), 0600)
					}
				case "tamper":
					err = os.WriteFile(output, []byte("wrong secret"), 0600)
				case "public":
					err = os.Chmod(output, 0644)
				case "hardlink":
					err = os.Link(output, filepath.Join(directory, "second"))
				}
				if err != nil {
					t.Errorf("custody scenario failed: %v", err)
				}
			}, nil)
			before := f.writes
			keys := len(f.legacy.cfg.APIKeys)
			result, err := s.operatorAdminKey(context.Background(), userID, "", output, nil)
			if err == nil || result.KeyID != "" {
				t.Fatal("unsafe custody reported success")
			}
			if mutation == "after-submit" || mutation == "tamper" {
				if len(f.legacy.cfg.APIKeys) != keys+1 {
					t.Fatal("post-submit scenario absent")
				}
			} else if f.writes != before || len(f.legacy.cfg.APIKeys) != keys {
				t.Fatal("unsafe custody activated key")
			}
			if mutation == "existing" {
				raw, _ := os.ReadFile(output)
				if string(raw) != "preserved" {
					t.Fatal("existing key file overwritten")
				}
			}
		})
	}
}

func TestOperatorAdminKeyUnknownCommitRecovery(t *testing.T) {
	f, s, userID, _ := operatorKeyFixture(t)
	originalUsers, _ := json.Marshal(f.legacy.cfg.Users)
	output := filepath.Join(t.TempDir(), "uncertain.key")
	keys := len(f.legacy.cfg.APIKeys)
	f.fail = "auth-readback"
	result, err := s.operatorAdminKey(context.Background(), userID, "", output, nil)
	if err == nil || result.KeyID != "" || len(f.legacy.cfg.APIKeys) != keys+1 {
		t.Fatal("unknown commit scenario absent or claimed success")
	}
	token := savedOperatorKey(t, output)
	f.legacy.corruptConfig = false
	f.fail = ""
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", token, ""), 200)
	keyID, _, _ := strings.Cut(token, ".")
	status(t, appRequest(t, s, "GET", "/api/v3/tokens/"+keyID, token, ""), 200)
	actualUsers, _ := json.Marshal(f.legacy.cfg.Users)
	if string(originalUsers) != string(actualUsers) || len(f.legacy.cfg.APIKeys) != keys+1 {
		t.Fatal("reconciliation reissued key or changed user")
	}
}

func TestOperatorAdminKeyCommandIsolation(t *testing.T) {
	f, s, userID, _ := operatorKeyFixture(t)
	setBootstrap(t, f.legacy)
	t.Setenv("GANGOLF_DIVOID_API_URL", s.graph.base.String())
	for _, name := range []string{"GANGOLF_MEDIA_DIVOID_TOKEN_FILE", "GANGOLF_MEDIA_ROOT_ID", "GANGOLF_MEDIA_CATALOG_ID", "GANGOLF_MEDIA_TENANT_USER_ID"} {
		t.Setenv(name, "")
	}
	output := filepath.Join(t.TempDir(), "command.key")
	previousArgs := os.Args
	defer func() { os.Args = previousArgs }()
	os.Args = []string{"gangolf-api", "admin-key", "--user-id", userID, "--output", output}
	printed, err := operatorCommandOutput(t, run)
	if err != nil {
		t.Fatalf("isolated command failed: %v", err)
	}
	token := savedOperatorKey(t, output)
	var result operatorKey
	if json.Unmarshal(printed, &result) != nil || result.UserID != userID || result.KeyID == "" || result.ExpiresAt.IsZero() || result.File != output || strings.Contains(string(printed), token) {
		t.Fatal("command output omitted metadata or disclosed secret")
	}
	explicit := time.Now().Add(48 * time.Hour).In(time.FixedZone("offset", 3600))
	second := filepath.Join(t.TempDir(), "explicit.key")
	printed, err = operatorCommandOutput(t, func() error {
		return adminKeyCommand([]string{"--user-id", userID, "--output", second, "--expires-at", explicit.Format(time.RFC3339Nano)})
	})
	secondToken := savedOperatorKey(t, second)
	if err != nil || json.Unmarshal(printed, &result) != nil || !result.ExpiresAt.Equal(explicit) || result.ExpiresAt.Location() != time.UTC || strings.Contains(string(printed), secondToken) {
		t.Fatal("explicit command expiry or output invalid")
	}
	writes := f.writes
	for _, args := range [][]string{{}, {"--user-id", userID}, {"--user-id", "01", "--output", output}, {"--user-id", userID, "--output", output, "--expires-at", ""}, {"--user-id", userID, "--output", output, "--expires-at", "bad"}, {"--user-id", userID, "--output", output, "--unknown"}, {"--user-id", userID, "--output", output, "extra"}} {
		if adminKeyCommand(args) == nil || f.writes != writes {
			t.Fatal("invalid command flags mutated state")
		}
	}
}

func TestHTTPTokenIssuerExpiryCapUnchanged(t *testing.T) {
	f, s := newResourceFixture(t)
	expiry := s.now().Add(2 * time.Hour)
	key := f.legacy.cfg.APIKeys["key"]
	key.ExpiresAt = &expiry
	f.legacy.cfg.APIKeys["key"] = key
	hostID := createAppTestUser(t, s, "Host")
	host := issueAppTestToken(t, s, hostID, `{}`)
	hostChannel := createAppTestChannel(t, s, host, "host")
	adminChannel := createAppTestChannel(t, s, "key.secret", "admin")
	for _, test := range []struct{ user, token, channel, permissions string }{{"7", "key.secret", adminChannel, `["admin","host"]`}, {hostID, host, hostChannel, `["host"]`}} {
		for _, scope := range []string{"api", "channel-read"} {
			input := map[string]any{"scope": scope}
			if scope == "api" {
				var permissions []string
				json.Unmarshal([]byte(test.permissions), &permissions)
				input["permissions"] = permissions
			} else {
				input["channelIds"] = []string{test.channel}
			}
			raw, _ := json.Marshal(input)
			row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/"+test.user+"/tokens", test.token, string(raw)), 201)
			expires, err := time.Parse(time.RFC3339Nano, row["expiresAt"].(string))
			if err != nil || !expires.Equal(expiry) {
				t.Fatal("HTTP issuer deadline was amplified")
			}
			input["expiresAt"] = expiry.Add(time.Hour)
			raw, _ = json.Marshal(input)
			status(t, appRequest(t, s, "POST", "/api/v3/users/"+test.user+"/tokens", test.token, string(raw)), 400)
		}
	}
}

func operatorCommandOutput(t *testing.T, command func() error) ([]byte, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	defer func() { os.Stdout = previous; reader.Close(); writer.Close() }()
	os.Stdout = writer
	commandErr := command()
	os.Stdout = previous
	writer.Close()
	printed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return printed, commandErr
}

func TestOperatorAdminKeyWithoutPreviousAppKeys(t *testing.T) {
	f, s, userID, _ := operatorKeyFixture(t)
	f.legacy.cfg.APIKeys = map[string]keyGrant{}
	before, _ := json.Marshal(f.legacy.cfg.Users)
	output := filepath.Join(t.TempDir(), "recovered.key")
	result, err := s.operatorAdminKey(context.Background(), userID, "", output, nil)
	if err != nil || len(f.legacy.cfg.APIKeys) != 1 || result.ExpiresAt.IsZero() {
		t.Fatal("operator recovery required an old app key")
	}
	token := savedOperatorKey(t, output)
	status(t, appRequest(t, s, "GET", "/api/v3/users", token, ""), 200)
	after, _ := json.Marshal(f.legacy.cfg.Users)
	if string(before) != string(after) {
		t.Fatal("recovery rewrote users")
	}
}
