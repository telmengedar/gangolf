package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func setBootstrap(t *testing.T, f *graphFixture) {
	t.Helper()
	values := map[string]string{"GANGOLF_DIVOID_API_URL": f.host.URL + "/api", "GANGOLF_DIVOID_TOKEN": "storage-only-test-token", "GANGOLF_API_KEY_PEPPER": "test-pepper", "GANGOLF_DIVOID_ROOT_ID": "10", "GANGOLF_DIVOID_MESSAGES_ID": "11", "GANGOLF_DIVOID_CONFIG_ID": "12", "GANGOLF_LISTEN": "127.0.0.1:0"}
	for k, v := range values {
		t.Setenv(k, v)
	}
}

func TestBootstrapRequiresExplicitCoordinatesAndSecrets(t *testing.T) {
	f, _ := newFixture(t)
	setBootstrap(t, f)
	if _, _, err := bootstrap(); err != nil {
		t.Fatal("valid bootstrap failed")
	}
	for _, key := range []string{"GANGOLF_DIVOID_API_URL", "GANGOLF_DIVOID_TOKEN", "GANGOLF_API_KEY_PEPPER", "GANGOLF_DIVOID_ROOT_ID", "GANGOLF_DIVOID_MESSAGES_ID", "GANGOLF_DIVOID_CONFIG_ID"} {
		t.Run(key, func(t *testing.T) {
			old := os.Getenv(key)
			t.Setenv(key, "")
			if _, _, err := bootstrap(); err == nil {
				t.Fatal("missing setting accepted")
			}
			t.Setenv(key, old)
		})
	}
	for _, target := range []string{"https://user:password@example.test/api", "http://example.test/api", "https://example.test/api?credential=bad", "https://example.test/api#fragment"} {
		t.Run(target, func(t *testing.T) {
			t.Setenv("GANGOLF_DIVOID_API_URL", target)
			if _, _, err := bootstrap(); err == nil {
				t.Fatal("unsafe base accepted")
			}
		})
	}
	t.Setenv("GANGOLF_DIVOID_CONFIG_ID", "11")
	if _, _, err := bootstrap(); err == nil {
		t.Fatal("overlapping IDs accepted")
	}
}

func TestRunServiceProcessHelper(t *testing.T) {
	if os.Getenv("GANGOLF_TEST_RUN_SERVICE") != "1" {
		return
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestRunAppendsAndReadsAfterProcessRestart(t *testing.T) {
	f, _ := newFixture(t)
	setBootstrap(t, f)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	start := func() (string, func()) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		listener.Close()
		cmd := exec.Command(binary, "-test.run=^TestRunServiceProcessHelper$")
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		cmd.Env = append(os.Environ(), "GANGOLF_TEST_RUN_SERVICE=1", "GANGOLF_LISTEN="+addr)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cmd.Process.Signal(syscall.SIGTERM)
			if err := cmd.Wait(); err != nil {
				t.Errorf("service shutdown: %v: %s", err, output.String())
			}
		}
		t.Cleanup(func() {
			if !stopped {
				cmd.Process.Kill()
				cmd.Wait()
			}
		})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			res, err := client.Get("http://" + addr + "/api/i1/health")
			if err == nil {
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode == 200 {
					return "http://" + addr, stop
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("service did not become live")
		return "", stop
	}
	base, stop := start()
	send := func(method, base, body string) (int, string) {
		t.Helper()
		target, err := url.JoinPath(base, "api", "v2", "messages")
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer key.secret")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(data)
	}
	code, body := send("POST", base, "["+testMessage+"]")
	if code != 201 {
		t.Fatalf("POST=%d body=%s", code, body)
	}
	stop()
	base, stop = start()
	defer stop()
	code, body = send("GET", base, "")
	if code != 200 || !strings.Contains(body, `"name":"Distinct Speaker"`) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("GET after restart=%d body=%s", code, body)
	}
	f.mu.Lock()
	uploads := f.uploads
	f.mu.Unlock()
	if uploads != 1 {
		t.Fatalf("uploads=%d", uploads)
	}
}

func TestGraphDeadlineReturnsUnavailable(t *testing.T) {
	f, s := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.graph.list(ctx)
	if err != errUnavailable {
		t.Fatalf("error=%v", err)
	}
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	if writes != 0 {
		t.Fatalf("writes=%s", strconv.Itoa(writes))
	}
}
