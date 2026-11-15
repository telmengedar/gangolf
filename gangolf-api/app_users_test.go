package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func appRequest(t *testing.T, s *server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func appRow(t *testing.T, w *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	status(t, w, want)
	var row map[string]any
	if json.Unmarshal(w.Body.Bytes(), &row) != nil {
		t.Fatal("invalid resource")
	}
	return row
}

func createAppTestUser(t *testing.T, s *server, name string) string {
	t.Helper()
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/users", "key.secret", `{"name":"`+name+`","enabled":true,"permissions":["host"]}`), 201)
	return row["id"].(string)
}

func issueAppTestToken(t *testing.T, s *server, userID, body string) string {
	t.Helper()
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/"+userID+"/tokens", "key.secret", body), 201)
	return row["token"].(string)
}

func createAppTestChannel(t *testing.T, s *server, token, channel string) string {
	t.Helper()
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/channels", token, `{"service":"Local","channel":"`+channel+`"}`), 201)
	return row["id"].(string)
}

func TestAppUserAdminAndSelf(t *testing.T) {
	f, s := newResourceFixture(t)
	id := createAppTestUser(t, s, "Alice")
	token := issueAppTestToken(t, s, id, `{}`)
	row := appRow(t, appRequest(t, s, "GET", "/api/v3/users/current", token, ""), 200)
	if row["name"] != "Alice" || row["id"] != id {
		t.Fatal("created user mismatch")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/users", token, ""), 403)
	status(t, appRequest(t, s, "GET", "/api/v3/users/7", token, ""), 403)
	status(t, appRequest(t, s, "PUT", "/api/v3/users/current", token, `{"name":"Changed","enabled":true,"permissions":["admin"]}`), 403)
	status(t, appRequest(t, s, "PUT", "/api/v3/users/current", token, `{"name":"Changed","enabled":true,"permissions":["host"]}`), 200)
	status(t, appRequest(t, s, "DELETE", "/api/v3/users/current", token, ""), 403)
	status(t, appRequest(t, s, "PUT", "/api/v3/users/7", "key.secret", `{"name":"Admin","enabled":false,"permissions":["admin"]}`), 409)
	status(t, appRequest(t, s, "DELETE", "/api/v3/users/7", "key.secret", ""), 409)
	status(t, appRequest(t, s, "PUT", "/api/v3/users/"+id, "key.secret", `{"name":"Changed","enabled":false,"permissions":["host"]}`), 200)
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", token, ""), 401)
	status(t, appRequest(t, s, "GET", "/api/v3/users/"+id, "key.secret", ""), 200)
	status(t, appRequest(t, s, "DELETE", "/api/v3/users/"+id, "key.secret", ""), 204)
	status(t, appRequest(t, s, "GET", "/api/v3/users/"+id, "key.secret", ""), 404)
	if f.legacy.cfg.Users[id].Enabled {
		t.Fatal("deleted user enabled")
	}
	for _, key := range f.legacy.cfg.APIKeys {
		if key.UserID == id {
			t.Fatal("deleted user key retained")
		}
	}
	status(t, appRequest(t, s, "POST", "/api/v3/users", "key.secret", `{"name":"x","name":"y","enabled":true,"permissions":["host"]}`), 400)
	status(t, appRequest(t, s, "POST", "/api/v3/users", "key.secret", `null`), 400)
	status(t, appRequest(t, s, "PATCH", "/api/v3/users/7", "key.secret", `{}`), 405)
}

func TestLegacyUserProfileAdoption(t *testing.T) {
	f, s := newResourceFixture(t)
	user := f.legacy.cfg.Users["7"]
	user.Permissions = []string{"host"}
	f.legacy.cfg.Users["7"] = user
	key := f.legacy.cfg.APIKeys["key"]
	key.Permissions = []string{"host"}
	f.legacy.cfg.APIKeys["key"] = key
	before := key
	row := appRow(t, appRequest(t, s, "GET", "/api/v3/users/current", "key.secret", ""), 200)
	if row["name"] != nil {
		t.Fatal("legacy profile invented")
	}
	id := createAppTestChannel(t, s, "key.secret", "adopted")
	grant := f.legacy.cfg.Users["7"]
	if grant.ProfileNodeID == "" || grant.ManagedOnly || !reflect.DeepEqual(before, f.legacy.cfg.APIKeys["key"]) || !reflect.DeepEqual(grant.Permissions, user.Permissions) {
		t.Fatal("legacy authority changed")
	}
	nodeID, _ := resourceID(id)
	root, _ := resourceID(grant.ProfileNodeID)
	if f.nodes[nodeID].Root != root || f.nodes[nodeID].Owner != 20 || root == 7 {
		t.Fatal("app ownership mixed with storage principal")
	}
	status(t, appRequest(t, s, "PUT", "/api/v3/users/current", "key.secret", `{"name":"Legacy","enabled":true,"permissions":["host"]}`), 200)
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", "key.secret", ""), 200)
}

func TestAppTokenLifecycleAndScope(t *testing.T) {
	f, s := newResourceFixture(t)
	id := createAppTestUser(t, s, "Alice")
	token := issueAppTestToken(t, s, id, `{}`)
	keyID, _, _ := strings.Cut(token, ".")
	stored := f.legacy.cfg.APIKeys[keyID]
	hash := hmac.New(sha256.New, []byte(s.pepper))
	hash.Write([]byte(token))
	if !hmac.Equal(hash.Sum(nil), stored.Hash) || stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(s.now().Add(30*24*time.Hour)) {
		t.Fatal("token hash/expiry mismatch")
	}
	row := appRow(t, appRequest(t, s, "GET", "/api/v3/tokens/"+keyID, token, ""), 200)
	if row["token"] != nil || row["hash"] != nil || row["pepper"] != nil {
		t.Fatal("token disclosed again")
	}
	status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", token, `{"permissions":["admin"]}`), 403)
	status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", token, `{"expiresAt":"2100-01-01T00:00:00Z"}`), 400)
	expiry := s.now().Add(45 * 24 * time.Hour).Format(time.RFC3339Nano)
	status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", token, `{"expiresAt":"`+expiry+`"}`), 400)
	status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", token, `{"expiresAt":"2000-01-01T00:00:00Z"}`), 400)
	channel := createAppTestChannel(t, s, token, "one")
	read := issueAppTestToken(t, s, id, `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", read, ""), 403)
	status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", read, `{}`), 403)
	status(t, appRequest(t, s, "POST", "/api/v3/channels", read, `{"service":"Local","channel":"two"}`), 403)
	status(t, appRequest(t, s, "GET", "/api/v3/channels/"+channel+"?apiKey="+url.QueryEscape(read), "", ""), 200)
	status(t, appRequest(t, s, "DELETE", "/api/v3/tokens/"+keyID, token, ""), 204)
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", token, ""), 401)
	adminHost := addTestKey(f, s, "scoped", "7", "host")
	status(t, appRequest(t, s, "POST", "/api/v3/users", adminHost, `{"name":"x","enabled":true,"permissions":["host"]}`), 403)
	admin := f.legacy.cfg.Users["7"]
	admin.Permissions = []string{"host"}
	f.legacy.cfg.Users["7"] = admin
	status(t, appRequest(t, s, "GET", "/api/v3/users", "key.secret", ""), 403)
}

func TestAppUserBootstrapAdmin(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "exists", "symlink", "admin"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			user := f.legacy.cfg.Users["7"]
			user.Permissions = []string{"host"}
			f.legacy.cfg.Users["7"] = user
			key := f.legacy.cfg.APIKeys["key"]
			key.Permissions = []string{"host"}
			f.legacy.cfg.APIKeys["key"] = key
			before, _ := json.Marshal(f.legacy.cfg.APIKeys["key"])
			directory := t.TempDir()
			path := filepath.Join(directory, "admin.key")
			if mode == "unknown" {
				f.fail = "auth-readback"
			}
			if mode == "exists" {
				if err := os.WriteFile(path, []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				if err := os.Symlink(filepath.Join(directory, "target"), path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "admin" {
				user.Permissions = []string{"admin"}
				f.legacy.cfg.Users["7"] = user
			}
			id, keyID, err := s.bootstrapAdmin(context.Background(), "Admin", path)
			if mode == "exists" || mode == "symlink" || mode == "admin" {
				if err == nil {
					t.Fatal("unsafe bootstrap accepted")
				}
				if len(f.nodes) != 0 {
					t.Fatal("unsafe bootstrap mutated graph")
				}
				return
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			token := strings.TrimSpace(string(raw))
			info, _ := os.Lstat(path)
			if info.Mode().Perm() != 0600 || !info.Mode().IsRegular() || len(token) < 60 {
				t.Fatal("credential file unsafe or incomplete")
			}
			if mode == "unknown" {
				if err == nil || id != "" || keyID != "" {
					t.Fatal("unknown write claimed success")
				}
				f.legacy.corruptConfig = false
				status(t, appRequest(t, s, "GET", "/api/v3/users/current", token, ""), 200)
				_, _, retryErr := s.bootstrapAdmin(context.Background(), "Retry", filepath.Join(directory, "retry.key"))
				if retryErr == nil {
					t.Fatal("committed bootstrap retried")
				}
			} else {
				if err != nil || id == "7" || keyID == "" {
					t.Fatalf("bootstrap failed: %v", err)
				}
				status(t, appRequest(t, s, "GET", "/api/v3/users/current", token, ""), 200)
			}
			after, _ := json.Marshal(f.legacy.cfg.APIKeys["key"])
			if string(before) != string(after) || !reflect.DeepEqual(f.legacy.cfg.Users["7"].Permissions, []string{"host"}) {
				t.Fatal("bootstrap changed existing authority")
			}
		})
	}
}

func TestUserSecurityWriteConfirmation(t *testing.T) {
	for _, mode := range []string{"auth-write", "auth-written", "auth-readback"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			id := createAppTestUser(t, s, "Alice")
			f.fail = mode
			response := appRequest(t, s, "POST", "/api/v3/users/"+id+"/tokens", "key.secret", `{}`)
			if mode == "auth-written" {
				status(t, response, 201)
			} else {
				status(t, response, 503)
				if strings.Contains(response.Body.String(), "token") {
					t.Fatal("unconfirmed token disclosed")
				}
			}
		})
	}
}

func TestAppUserBootstrapAdminCommand(t *testing.T) {
	f, _ := newFixture(t)
	f.mu.Lock()
	user := f.cfg.Users["7"]
	user.Permissions = []string{"host"}
	f.cfg.Users["7"] = user
	f.mu.Unlock()
	resource, s := newResourceFixture(t)
	resource.legacy = f
	setBootstrap(t, f)
	t.Setenv("GANGOLF_DIVOID_API_URL", s.graph.base.String())
	t.Setenv("GANGOLF_MEDIA_DIVOID_TOKEN_FILE", "")
	t.Setenv("GANGOLF_MEDIA_ROOT_ID", "")
	t.Setenv("GANGOLF_MEDIA_CATALOG_ID", "")
	t.Setenv("GANGOLF_MEDIA_TENANT_USER_ID", "")
	output := filepath.Join(t.TempDir(), "admin.key")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	err = bootstrapAdminCommand([]string{"--name", "Administrator", "--output", output})
	os.Stdout = previous
	writer.Close()
	printed, readErr := io.ReadAll(reader)
	reader.Close()
	if err != nil || readErr != nil {
		t.Fatalf("command failed: %v", err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if json.Unmarshal(printed, &result) != nil || result["userId"] == "" || result["keyId"] == "" || result["file"] != output || strings.Contains(string(printed), strings.TrimSpace(string(raw))) {
		t.Fatal("command output leaked credential or omitted confirmed metadata")
	}
	if resource.writes == 0 {
		t.Fatal("bootstrap command did not use graph fixture")
	}
}

func TestAppUserProfileBindingFailsClosed(t *testing.T) {
	for _, mutation := range []string{"deleted", "wrong-user", "wrong-root", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			f, s := newResourceFixture(t)
			id := createAppTestUser(t, s, "Alice")
			token := issueAppTestToken(t, s, id, `{}`)
			nodeID := mustResourceID(f.legacy.cfg.Users[id].ProfileNodeID)
			n := f.nodes[nodeID]
			var profile appUser
			json.Unmarshal([]byte(*n.Content), &profile)
			switch mutation {
			case "deleted":
				profile.Deleted = true
			case "wrong-user":
				profile.UserID = "999"
			case "wrong-root":
				n.Root = 999
			case "missing":
				delete(f.nodes, nodeID)
			}
			if mutation != "missing" {
				raw, _ := json.Marshal(profile)
				n.Content = new(string(raw))
				f.nodes[nodeID] = n
			}
			response := appRequest(t, s, "GET", "/api/v3/users/current/tokens", token, "")
			if response.Code < 400 || strings.Contains(response.Body.String(), `"result"`) {
				t.Fatal("invalid profile binding authenticated")
			}
		})
	}
}

func TestAppUserUnconfirmedGrantDoesNotActivateProfile(t *testing.T) {
	f, s := newResourceFixture(t)
	f.fail = "auth-write"
	response := appRequest(t, s, "POST", "/api/v3/users", "key.secret", `{"name":"Orphan","enabled":true,"permissions":["host"]}`)
	status(t, response, 503)
	if len(f.nodes) != 1 || len(f.legacy.cfg.Users) != 1 {
		t.Fatal("unconfirmed grant activated or profile scenario absent")
	}
	for nodeID, n := range f.nodes {
		if n.Type != "gangolf-user" || n.Content == nil {
			t.Fatal("orphan profile was not persisted")
		}
		status(t, appRequest(t, s, "GET", "/api/v3/users/"+strconv.FormatInt(nodeID, 10), "key.secret", ""), 404)
	}
}
