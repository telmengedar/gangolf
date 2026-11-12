package main

import (
	"strings"
	"testing"
)

func TestV3ConfigurationAndSecretBoundary(t *testing.T) {
	for _, value := range []string{"", "0", "01", "-1", "1.0", "bot", " 99 99", "credential-sentinel"} {
		values := map[string]string{"GANGOLF_API_KEY": "key-sentinel", "GANGOLF_CHANNELS": "Twitch/123", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_TWITCH_BOT_USER_ID": value}
		_, err := loadConfig(env(values))
		if err == nil || !strings.Contains(err.Error(), "GANGOLF_TWITCH_BOT_USER_ID") || strings.Contains(err.Error(), "credential-sentinel") || strings.Contains(err.Error(), "key-sentinel") {
			t.Fatalf("err=%v", err)
		}
	}
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "key-sentinel", "GANGOLF_CHANNELS": "Twitch/123", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_TWITCH_BOT_USER_ID": "99"}))
	if err != nil || cfg.twitchBotUserID != "99" || cfg.memoryConfig != 88 || len(cfg.channels) != 1 || cfg.channels[0].Service != "Twitch" || cfg.channels[0].Channel != "123" || len(cfg.routes[cfg.channels[0]]) != 1 || cfg.routes[cfg.channels[0]][0] != cfg.channels[0] {
		t.Fatalf("configuration mismatch: %v", err)
	}
}
