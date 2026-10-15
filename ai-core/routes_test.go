package main

import (
	"strings"
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

var (
	hostKey    = window.Key{Service: "localhost", Channel: "host"}
	streamKey  = window.Key{Service: "twitch", Channel: "stream"}
	discordKey = window.Key{Service: "discord", Channel: "general"}
	otherKey   = window.Key{Service: "other", Channel: "room"}
)

func routesOf(channels, routes string) (config, error) {
	values := map[string]string{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": channels}
	if routes != "" {
		values["GANGOLF_ROUTES"] = routes
	}
	return loadConfig(env(values))
}

func mustRoute(t *testing.T, channels, routes string) map[window.Key][]window.Key {
	t.Helper()
	cfg, err := routesOf(channels, routes)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.routes
}

func requireRouteError(t *testing.T, channels, routes, phrase string) {
	t.Helper()
	_, err := routesOf(channels, routes)
	if err == nil || !strings.Contains(err.Error(), "GANGOLF_ROUTES") || !strings.Contains(err.Error(), phrase) {
		t.Fatalf("routes %q: err = %v, want GANGOLF_ROUTES named with %q", routes, err, phrase)
	}
}

func requireTargets(t *testing.T, routes map[window.Key][]window.Key, source window.Key, want ...window.Key) {
	t.Helper()
	got := routes[source]
	if len(got) != len(want) {
		t.Fatalf("route of %v = %v, want %v", source, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route of %v = %v, want %v", source, got, want)
		}
	}
}

func TestWithoutRoutesEveryChannelAnswersItself(t *testing.T) {
	routes := mustRoute(t, "twitch/stream, discord/general", "")

	if len(routes) != 2 {
		t.Fatalf("routes = %v, want one per channel", routes)
	}
	requireTargets(t, routes, streamKey, streamKey)
	requireTargets(t, routes, discordKey, discordKey)
}

func TestBlankRoutesCountAsUnset(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream", "GANGOLF_ROUTES": "   "}))

	if err != nil {
		t.Fatal(err)
	}
	requireTargets(t, cfg.routes, streamKey, streamKey)
}

func TestStarRoutesToEveryChannelThatAnswersItself(t *testing.T) {
	routes := mustRoute(t, "localhost/host,twitch/stream,discord/general", "localhost/host=*")

	requireTargets(t, routes, hostKey, streamKey, discordKey)
	requireTargets(t, routes, streamKey, streamKey)
	requireTargets(t, routes, discordKey, discordKey)
}

func TestStarLeavesOutAChannelRoutedElsewhere(t *testing.T) {
	routes := mustRoute(t, "localhost/host,twitch/stream,discord/general", "localhost/host=*,discord/general=twitch/stream")

	requireTargets(t, routes, hostKey, streamKey)
	requireTargets(t, routes, discordKey, streamKey)
}

func TestAListRouteKeepsExactlyItsTargets(t *testing.T) {
	routes := mustRoute(t, "localhost/host,twitch/stream,discord/general,other/room", "localhost/host=discord/general|twitch/stream")

	requireTargets(t, routes, hostKey, discordKey, streamKey)
	requireTargets(t, routes, otherKey, otherKey)
}

func TestASingleExplicitTargetReplacesTheDefault(t *testing.T) {
	routes := mustRoute(t, "localhost/host,twitch/stream", " localhost/host = twitch/stream ")

	requireTargets(t, routes, hostKey, streamKey)
}

func TestASourceMayRouteToItselfAndOthers(t *testing.T) {
	routes := mustRoute(t, "twitch/stream,discord/general", "twitch/stream=twitch/stream|discord/general")

	requireTargets(t, routes, streamKey, streamKey, discordKey)
	requireTargets(t, routes, discordKey, discordKey)
}

func TestAMalformedRouteEntryIsRejected(t *testing.T) {
	for _, bad := range []string{"justaname", "localhost/host", "localhost=twitch/stream", "=twitch/stream", "localhost/host=twitch/stream,", ",localhost/host=twitch/stream"} {
		requireRouteError(t, "localhost/host,twitch/stream", bad, "must look like service/channel=targets")
	}
}

func TestAMalformedRouteTargetIsRejected(t *testing.T) {
	for _, bad := range []string{"localhost/host=stream", "localhost/host=twitch/", "localhost/host=twitch/stream|", "localhost/host=|twitch/stream", "localhost/host=**"} {
		requireRouteError(t, "localhost/host,twitch/stream", bad, "targets must be * or service/channel joined by |")
	}
}

func TestARouteFromAnUnlistedSourceIsRejected(t *testing.T) {
	requireRouteError(t, "twitch/stream", "localhost/host=twitch/stream", "source is not in GANGOLF_CHANNELS")
}

func TestARouteListedTwiceForOneSourceIsRejected(t *testing.T) {
	requireRouteError(t, "localhost/host,twitch/stream,discord/general", "localhost/host=twitch/stream,localhost/host=discord/general", "source is listed twice")
}

func TestARouteWithoutTargetsIsRejected(t *testing.T) {
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=", "has no targets")
}

func TestARouteNamingATargetTwiceIsRejected(t *testing.T) {
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=twitch/stream|twitch/stream", "lists a target twice")
}

func TestARouteMixingStarWithExplicitTargetsIsRejected(t *testing.T) {
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=*|twitch/stream", "mixes * with explicit targets")
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=twitch/stream|*", "mixes * with explicit targets")
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=*|*", "mixes * with explicit targets")
}

func TestARouteToAnUnlistedTargetIsRejected(t *testing.T) {
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=discord/general", "target is not in GANGOLF_CHANNELS")
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=twitch/stream|discord/general", "target is not in GANGOLF_CHANNELS")
}

func TestARouteToAChannelRoutedElsewhereIsRejected(t *testing.T) {
	phrase := "target is routed to other channels and has no conversation of its own"
	requireRouteError(t, "localhost/host,twitch/stream,discord/general", "localhost/host=discord/general,discord/general=twitch/stream", phrase)
	requireRouteError(t, "localhost/host,twitch/stream", "localhost/host=twitch/stream,twitch/stream=localhost/host", phrase)
	requireRouteError(t, "localhost/host,twitch/stream,discord/general", "localhost/host=*,discord/general=localhost/host", phrase)
}

func TestARouteToAChannelThatAnswersItselfThroughAnotherEntryIsAccepted(t *testing.T) {
	routes := mustRoute(t, "localhost/host,twitch/stream,discord/general", "localhost/host=discord/general,discord/general=discord/general|twitch/stream")

	requireTargets(t, routes, hostKey, discordKey)
	requireTargets(t, routes, discordKey, discordKey, streamKey)
}

func TestStarThatResolvesToNoChannelIsRejected(t *testing.T) {
	requireRouteError(t, "localhost/host", "localhost/host=*", "* resolves to no channel")
}

func TestStarNamesAChannelListedTwiceOnce(t *testing.T) {
	routes := mustRoute(t, "localhost/host,twitch/stream,twitch/stream", "localhost/host=*")

	requireTargets(t, routes, hostKey, streamKey)
}

func TestARouteErrorNamesTheEntryButNeverTheKey(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "keyid.topsecretvalue", "GANGOLF_CHANNELS": "twitch/stream", "GANGOLF_ROUTES": "localhost/host=twitch/stream"}))

	if err == nil || !strings.Contains(err.Error(), "localhost/host=twitch/stream") || strings.Contains(err.Error(), "topsecretvalue") {
		t.Fatalf("err = %v, want the offending entry named and no secret", err)
	}
}
