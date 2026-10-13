package main

import (
	"log/slog"
	"strings"
	"testing"
)

func TestSetAndDefaultedVariablesAreNamedInOrder(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GANGOLF_API_KEY":   "k.s",
		"GANGOLF_CHANNELS":  "twitch/stream",
		"PROCESSOR_SUBJECT": "42",
	}))

	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.set, ",") != "GANGOLF_API_KEY,GANGOLF_CHANNELS,PROCESSOR_SUBJECT" {
		t.Fatalf("set = %v", cfg.set)
	}
	if strings.Join(cfg.defaulted, ",") != "GANGOLF_API_URL,GANGOLF_BOT_NAME,PROCESSOR_URL" {
		t.Fatalf("defaulted = %v", cfg.defaulted)
	}
}

func TestStartupLogNamesVariablesAndNeverPrintsValues(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GANGOLF_API_URL":   "https://user:hunter2@example.invalid/api",
		"GANGOLF_API_KEY":   "keyid.topsecretvalue",
		"GANGOLF_CHANNELS":  "twitch/privatechannel",
		"GANGOLF_BOT_NAME":  "Distinctivebot",
		"PROCESSOR_URL":     "http://internal-host.invalid:8080",
		"PROCESSOR_SUBJECT": "424242",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder

	logStartup(slog.New(slog.NewTextHandler(&logs, nil)), cfg)

	out := logs.String()
	for _, value := range []string{"hunter2", "topsecretvalue", "privatechannel", "Distinctivebot", "internal-host", "424242", "example.invalid"} {
		if strings.Contains(out, value) {
			t.Fatalf("startup log printed a configured value %q: %s", value, out)
		}
	}
	for _, name := range []string{"GANGOLF_API_KEY", "GANGOLF_API_URL", "PROCESSOR_SUBJECT"} {
		if !strings.Contains(out, name) {
			t.Fatalf("startup log = %s, want the variable name %s", out, name)
		}
	}
	if !strings.Contains(out, "channels=1") {
		t.Fatalf("startup log = %s, want the channel count", out)
	}
}
