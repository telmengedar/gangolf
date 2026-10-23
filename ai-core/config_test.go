package main

import (
	"strings"
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestDefaultsApplyWhenOnlyTheRequiredVariablesAreSet(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_CHANNELS": "twitch/stream"}))

	if err != nil {
		t.Fatal(err)
	}
	if cfg.apiURL != "https://gangolf.mamgo.io/api" || cfg.botName != "GangolfBot" || cfg.processorURL != "http://gangolf:8080" || cfg.subject != 1090 || cfg.processorConfig != 17607 || cfg.memoryConfig != 88 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestEveryVariableOverridesItsDefault(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GANGOLF_API_URL":         "https://api.example.invalid/x",
		"GANGOLF_API_KEY":         "k.s",
		"PROCESSOR_MEMORY_CONFIG": "88",
		"GANGOLF_CHANNELS":        "twitch/stream, discord/general",
		"GANGOLF_BOT_NAME":        "Gangolf",
		"PROCESSOR_URL":           "http://processor.example.invalid:9",
		"PROCESSOR_SUBJECT":       "42",
		"PROCESSOR_CONFIG":        "99",
	}))

	if err != nil {
		t.Fatal(err)
	}
	want := []window.Key{{Service: "twitch", Channel: "stream"}, {Service: "discord", Channel: "general"}}
	if len(cfg.channels) != 2 || cfg.channels[0] != want[0] || cfg.channels[1] != want[1] {
		t.Fatalf("channels = %v, want %v", cfg.channels, want)
	}
	if cfg.apiURL != "https://api.example.invalid/x" || cfg.apiKey != "k.s" || cfg.botName != "Gangolf" || cfg.processorURL != "http://processor.example.invalid:9" || cfg.subject != 42 || cfg.processorConfig != 99 || cfg.memoryConfig != 88 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestMissingRequiredVariablesAreNamedInTheError(t *testing.T) {
	_, noKey := loadConfig(env(map[string]string{"GANGOLF_CHANNELS": "twitch/stream"}))
	_, noChannels := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s"}))

	if noKey == nil || !strings.Contains(noKey.Error(), "GANGOLF_API_KEY") {
		t.Fatalf("err = %v, want the key variable named", noKey)
	}
	if noChannels == nil || !strings.Contains(noChannels.Error(), "GANGOLF_CHANNELS") {
		t.Fatalf("err = %v, want the channels variable named", noChannels)
	}
}

func TestBlankRequiredVariableCountsAsMissing(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "   ", "GANGOLF_CHANNELS": "twitch/stream"}))

	if err == nil {
		t.Fatal("a blank key must be rejected")
	}
}

func TestMalformedChannelEntriesAreRejectedWithoutEchoingTheValue(t *testing.T) {
	for _, bad := range []string{"justaname", "twitch/", "/stream", "twitch/stream,broken"} {
		_, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_CHANNELS": bad}))
		if err == nil || strings.Contains(err.Error(), bad) {
			t.Fatalf("channels %q: err = %v, want a rejection that does not echo the value", bad, err)
		}
	}
}

func TestInvalidSubjectIsRejectedWithoutEchoingTheValue(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-5", "99999999999999999999"} {
		_, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_SUBJECT": bad}))
		if err == nil || !strings.Contains(err.Error(), "PROCESSOR_SUBJECT") || strings.Contains(err.Error(), bad) {
			t.Fatalf("subject %q: err = %v, want the variable named and the value not echoed", bad, err)
		}
	}
}

func TestInvalidProcessorConfigIsRejectedWithoutEchoingTheValue(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-5", "99999999999999999999"} {
		_, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_CONFIG": bad}))
		if err == nil || !strings.Contains(err.Error(), "PROCESSOR_CONFIG") || strings.Contains(err.Error(), bad) {
			t.Fatalf("config %q: err = %v, want the variable named and the value not echoed", bad, err)
		}
	}
}

func TestMissingKeyErrorNeverContainsAnyProvidedSecret(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"GANGOLF_API_URL": "https://user:hunter2@example.invalid", "GANGOLF_CHANNELS": "twitch/stream"}))

	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v", err)
	}
}
