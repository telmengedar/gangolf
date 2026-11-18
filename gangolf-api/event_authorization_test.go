package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamEvents_ScopedFrame_ReusesFinalChannelChecks(t *testing.T) {
	for _, kind := range []string{"message.created", "video.triggered"} {
		t.Run(kind, func(t *testing.T) {
			_, _, s, _, channel := automaticVideoFixture(t)
			token := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
			var primaryReads, mediaReads, channelReads atomic.Int64
			original := s.graph.http
			s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
				primaryReads.Add(1)
				if r.URL.Query().Get("type") == "channel" {
					channelReads.Add(1)
				}
				time.Sleep(2 * time.Millisecond)
				return nil
			}}}
			mediaOriginal := s.media.graph.http
			s.media.graph.http = &http.Client{Transport: videoTestTransport{base: mediaOriginal.Transport, before: func(r *http.Request) error {
				mediaReads.Add(1)
				time.Sleep(2 * time.Millisecond)
				return nil
			}}}
			stream := openEventTestStream(t, eventTestHost(t, s), token, "types="+kind+"&service=Local&channel=owned")
			defer s.eventHub().shutdown()
			initialChannels := channelReads.Swap(0)
			if initialChannels == 0 {
				t.Fatal("scoped handshake did not check channels")
			}
			primaryReads.Store(0)
			mediaReads.Store(0)
			started := time.Now()
			s.publishEvent(kind, "Local", "owned", &channel, map[string]string{"control": "delivered"})
			envelope, payload := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
			if envelope.Type != kind || string(payload["control"]) != `"delivered"` {
				t.Fatal("positive scoped frame not delivered")
			}
			t.Logf("primary_reads=%d media_reads=%d channel_reads=%d injected_per_read_ms=2 published_to_frame_ms=%.2f", primaryReads.Load(), mediaReads.Load(), channelReads.Load(), float64(time.Since(started).Microseconds())/1000)
			if channelReads.Load() != initialChannels {
				t.Fatalf("frame repeated channel work: handshake=%d frame=%d", initialChannels, channelReads.Load())
			}
		})
	}
}

func TestStreamEvents_MemoizedSourceIdentityAndNextRevocation_FailsClosed(t *testing.T) {
	for _, kind := range []string{"message.created", "video.triggered"} {
		t.Run(kind, func(t *testing.T) {
			_, f, s, _, channel := automaticVideoFixture(t)
			token := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
			stream := openEventTestStream(t, eventTestHost(t, s), token, "types="+kind)
			defer s.eventHub().shutdown()
			for _, source := range []struct {
				service string
				channel string
			}{{"Twitch", "owned"}, {"Local", "other"}} {
				s.publishEvent(kind, source.service, source.channel, &channel, map[string]string{"control": "wrong-identity"})
			}
			s.publishEvent(kind, "Local", "owned", &channel, map[string]string{"control": "positive-barrier"})
			_, payload := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
			if string(payload["control"]) != `"positive-barrier"` {
				t.Fatal("cached channel admitted a mismatched service or channel")
			}
			f.mu.Lock()
			node := f.nodes[mustResourceID(channel)]
			var identity identityContent
			if json.Unmarshal([]byte(*node.Content), &identity) != nil {
				t.Fatal("invalid positive-control channel")
			}
			identity.Enabled = new(false)
			data, _ := json.Marshal(identity)
			node.Content = new(string(data))
			f.nodes[node.ID] = node
			f.mu.Unlock()
			s.publishEvent(kind, "Local", "owned", &channel, map[string]string{"control": "must-not-leak"})
			select {
			case frame, ok := <-stream.frames:
				if ok {
					t.Fatalf("revoked channel emitted frame kind=%s", strings.SplitN(frame, "\n", 2)[0])
				}
			case <-time.After(time.Second):
				t.Fatal("revoked channel did not close stream")
			}
		})
	}
}
