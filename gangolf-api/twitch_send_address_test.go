package main

import (
	"strings"
	"testing"
)

func TestSendAddress_EndpointComposition_ContainsFakeSends(t *testing.T) {
	for _, mode := range []string{"derived", "explicit-local", "public-send", "foreign-port", "mixed-validate", "mixed-token", "mixed-subscription", "mixed-socket", "user-info", "query", "fragment", "public-subdomain", "zero-address", "production"} {
		t.Run(mode, func(t *testing.T) {
			_, s, tw := outboundFixture(t)
			want := tw.host.URL + "/helix/chat/messages"
			allowed := false
			switch mode {
			case "derived":
				allowed = true
			case "explicit-local":
				tw.client.sendURL = tw.host.URL + "/custom-send"
				want = tw.client.sendURL
				allowed = true
			case "public-send":
				tw.client.sendURL = "https://example.invalid/send"
			case "foreign-port":
				tw.client.sendURL = "http://127.0.0.1:1/send"
			case "mixed-validate":
				tw.client.validateURL = twitchValidateAddress
			case "mixed-token":
				tw.client.tokenURL = twitchTokenAddress
			case "mixed-subscription":
				tw.client.subscriptionURL = twitchSubscriptionAddress
			case "mixed-socket":
				tw.client.socketURL = twitchSocketAddress
			case "user-info":
				tw.client.sendURL = strings.Replace(tw.host.URL, "http://", "http://user:secret@", 1) + "/send"
			case "query":
				tw.client.sendURL = tw.host.URL + "/send?target=public"
			case "fragment":
				tw.client.sendURL = tw.host.URL + "/send#fragment"
			case "public-subdomain":
				tw.client.subscriptionURL = "http://127.0.0.1.example.invalid/subscriptions"
			case "zero-address":
				tw.client.subscriptionURL = "http://0.0.0.0/subscriptions"
			case "production":
				client := productionTwitchClient("")
				address, err := client.sendAddress()
				if err != nil || address != twitchSendAddress {
					t.Fatal("canonical production binding changed")
				}
				return
			}
			address, err := tw.client.sendAddress()
			if allowed {
				if err != nil || address != want {
					t.Fatalf("address=%s error=%v", address, err)
				}
				if mode == "derived" {
					status(t, outboundRequest(s, "key.secret", "local", outboundBody), 201)
					if sends(tw) != 1 {
						t.Fatal("derived fake endpoint not reached")
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("unsafe composition accepted: %s", address)
			}
			w := outboundRequest(s, "key.secret", "blocked", outboundBody)
			status(t, w, 503)
			if sends(tw) != 0 || tw.validationCalls != 0 {
				t.Fatal("unsafe composition reached Twitch preflight/send")
			}
		})
	}
}
