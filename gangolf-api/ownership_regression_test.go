package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnedMessageReadLockDeadline(t *testing.T) {
	for _, path := range []string{"/api/v3/messages", "/api/v3/messages/123"} {
		t.Run(path, func(t *testing.T) {
			_, s := newResourceFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer key.secret")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			resourceLock.Lock()
			go func() { s.ServeHTTP(w, r); close(done) }()
			prompt := false
			select {
			case <-done:
				prompt = true
			case <-time.After(100 * time.Millisecond):
			}
			resourceLock.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("message reader did not finish after release")
			}
			if !prompt {
				t.Fatal("message read waited past request deadline until lock release")
			}
			status(t, w, 503)
		})
	}
}

func TestOwnedMessageReadWaitRechecksGrants(t *testing.T) {
	f, s := newResourceFixture(t)
	r := httptest.NewRequest("GET", "/api/v3/messages", nil)
	r.Header.Set("Authorization", "Bearer key.secret")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	resourceLock.Lock()
	go func() { s.ServeHTTP(w, r); close(done) }()
	f.legacy.mu.Lock()
	user := f.legacy.cfg.Users["7"]
	user.Enabled = false
	f.legacy.cfg.Users["7"] = user
	f.legacy.mu.Unlock()
	resourceLock.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiting reader did not finish")
	}
	status(t, w, 401)
}

func TestBootstrapOutputCustodyBeforeActivation(t *testing.T) {
	for _, mutation := range []string{"unlink", "replace", "public", "hardlink", "after-submit"} {
		t.Run(mutation, func(t *testing.T) {
			f, s := newResourceFixture(t)
			user := f.legacy.cfg.Users["7"]
			user.Permissions = []string{"host"}
			f.legacy.cfg.Users["7"] = user
			key := f.legacy.cfg.APIKeys["key"]
			key.Permissions = []string{"host"}
			f.legacy.cfg.APIKeys["key"] = key
			directory := t.TempDir()
			output := filepath.Join(directory, "bootstrap.key")
			changed := false
			observeIdentityWire(s, func(r *http.Request) {
				if changed {
					return
				}
				if mutation == "after-submit" && !(r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/12/content")) {
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
						err = os.WriteFile(output, []byte("replacement"), 0600)
					}
				case "public":
					err = os.Chmod(output, 0644)
				case "hardlink":
					err = os.Link(output, filepath.Join(directory, "second-link"))
				}
				if err != nil {
					t.Errorf("fixture custody mutation failed: %v", err)
				}
			}, nil)
			id, keyID, err := s.bootstrapAdmin(context.Background(), "Administrator", output)
			if !changed {
				t.Fatal("custody mutation scenario absent")
			}
			if err == nil || id != "" || keyID != "" {
				t.Fatal("bootstrap reported recoverable success after output custody loss")
			}
			if mutation != "after-submit" {
				if len(f.legacy.cfg.Users) != 1 || len(f.legacy.cfg.APIKeys) != 1 {
					t.Fatal("bootstrap activated authority without durable recoverable path")
				}
			} else {
				if len(f.legacy.cfg.Users) != 2 || len(f.legacy.cfg.APIKeys) != 2 {
					t.Fatal("post-submit scenario did not activate before final check")
				}
			}
		})
	}
}
