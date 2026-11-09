package main

import (
	"net/netip"
	"net/url"
)

const twitchValidateAddress = "https://id.twitch.tv/oauth2/validate"
const twitchTokenAddress = "https://id.twitch.tv/oauth2/token"
const twitchSubscriptionAddress = "https://api.twitch.tv/helix/eventsub/subscriptions"
const twitchSendAddress = "https://api.twitch.tv/helix/chat/messages"
const twitchSocketAddress = "wss://eventsub.wss.twitch.tv/ws"

func twitchLoopbackAddress(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return u, err == nil && ip.IsLoopback() && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func (c *twitchClient) sendAddress() (string, error) {
	if c.validateURL == twitchValidateAddress && c.tokenURL == twitchTokenAddress && c.subscriptionURL == twitchSubscriptionAddress && c.socketURL == twitchSocketAddress && c.sendURL == twitchSendAddress {
		return twitchSendAddress, nil
	}
	base, ok := twitchLoopbackAddress(c.subscriptionURL)
	if !ok || base.Scheme != "http" && base.Scheme != "https" {
		return "", errTwitchTransport
	}
	for _, raw := range []string{c.validateURL, c.tokenURL} {
		u, ok := twitchLoopbackAddress(raw)
		if !ok || u.Host != base.Host || u.Scheme != base.Scheme {
			return "", errTwitchTransport
		}
	}
	socketScheme := "ws"
	if base.Scheme == "https" {
		socketScheme = "wss"
	}
	socket, ok := twitchLoopbackAddress(c.socketURL)
	if !ok || socket.Host != base.Host || socket.Scheme != socketScheme {
		return "", errTwitchTransport
	}
	if c.sendURL == "" || c.sendURL == twitchSendAddress {
		base.Path, base.RawPath = "/helix/chat/messages", ""
		return base.String(), nil
	}
	destination, ok := twitchLoopbackAddress(c.sendURL)
	if !ok || destination.Host != base.Host || destination.Scheme != base.Scheme {
		return "", errTwitchTransport
	}
	return destination.String(), nil
}
