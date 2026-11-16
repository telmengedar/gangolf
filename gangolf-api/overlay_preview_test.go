package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestOverlayBrowserPreview(t *testing.T) {
	if os.Getenv("GANGOLF_TEST_OVERLAY_PREVIEW") != "1" {
		return
	}
	output := os.Getenv("GANGOLF_TEST_OVERLAY_PREVIEW_FILE")
	if output == "" {
		t.Fatal("preview output path required")
	}
	video := os.Getenv("GANGOLF_TEST_OVERLAY_VIDEO_ID")
	if !remoteVideoPattern.MatchString(video) {
		t.Fatal("preview needs a valid public fixture video ID")
	}
	media, _, s, sub, channel := automaticVideoFixture(t)
	s.eventHub().remove(sub)
	s.now = time.Now
	c := media.state(t)
	c.Clips[0].RemoteVideoID = video
	c.Clips[0].StartSeconds = new(1.0)
	c.Clips[0].EndSeconds = new(1.5)
	media.set(c)
	source := createAutomaticMessage(t, s, "preview source")
	read := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
	main, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	player, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()
	parentOrigin, playerOrigin := "http://"+main.Addr().String(), "http://"+player.Addr().String()
	t.Setenv("GANGOLF_OVERLAY_PLAYER_LISTEN_ADDR", player.Addr().String())
	t.Setenv("GANGOLF_OVERLAY_PLAYER_ORIGIN", playerOrigin)
	t.Setenv("GANGOLF_OVERLAY_PARENT_ORIGIN", parentOrigin)
	s.player, err = configureOverlayPlayer(main.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("preview output unavailable")
	}
	data := map[string]any{"parentOrigin": parentOrigin, "playerOrigin": playerOrigin, "readToken": read, "adminToken": "key.secret", "messageId": source.ID, "triggerId": "1", "channelId": channel, "service": "Local", "channel": "owned"}
	if json.NewEncoder(file).Encode(data) != nil {
		file.Close()
		t.Fatal("preview output failed")
	}
	if file.Sync() != nil || file.Close() != nil {
		t.Fatal("preview output failed")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := serveOverlayListeners(ctx, s, main, player); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("preview listener failed")
	}
}
