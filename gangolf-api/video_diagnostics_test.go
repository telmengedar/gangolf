package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func captureVideoDiagnostics(t *testing.T) chan map[string]any {
	t.Helper()
	records := make(chan map[string]any, 256)
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(videoDiagnosticWriter{records: records}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(original) })
	return records
}

func TestScheduleVideo_QueueAndSubscriberOverflow_LogsBoundedSanitizedReasons(t *testing.T) {
	records := captureVideoDiagnostics(t)
	_, s := newResourceFixture(t)
	s.matcher = newVideoMatcher(context.Background())
	for id := int64(1); id <= videoMatchQueue+2; id++ {
		s.scheduleVideo(id)
	}
	if s.matcher.dropped.Load() != 2 || len(s.matcher.queue) != 64 {
		t.Fatal("video overflow scenario was not reached")
	}
	sub, code := s.eventHub().subscribe("diagnostic", eventFilter{types: map[string]bool{"message.created": true}}, nil)
	if code != 0 {
		t.Fatal(code)
	}
	for range eventQueue + 1 {
		s.publishEvent("message.created", "Local", "room", nil, map[string]string{"private": "sensitive-payload-marker"})
	}
	if !sub.invalid || len(sub.queue) != 0 {
		t.Fatal("subscriber overflow scenario was not reached")
	}
	for _, message := range []string{"automatic video queue full", "event subscriber overflow"} {
		select {
		case record := <-records:
			if record["msg"] != message || record["reason"] != "queue_full" || record["queued"] != float64(64) {
				t.Fatalf("wrong bounded overflow diagnostic: %v", record)
			}
			data, _ := json.Marshal(record)
			if strings.Contains(string(data), "sensitive-payload-marker") {
				t.Fatal("diagnostic exposed event payload")
			}
		case <-time.After(time.Second):
			t.Fatal("overflow diagnostic missing")
		}
	}
	if len(records) != 0 {
		t.Fatal("repeated queue overflow produced unbounded warnings")
	}
}

func TestRunVideoMatcher_AttemptOutcomes_RecordsQueueWaitAndDeadline(t *testing.T) {
	for _, scenario := range []string{"no_match", "media_unavailable", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			records := captureVideoDiagnostics(t)
			media, _, s, sub, _ := automaticVideoFixture(t)
			message := createAutomaticMessage(t, s, "sensitive-payload-marker")
			var held chan struct{}
			switch scenario {
			case "media_unavailable":
				media.fail = "all"
			case "deadline":
				held = make(chan struct{}, 1)
				original := s.media.graph.http
				s.media.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
					held <- struct{}{}
					<-r.Context().Done()
					return r.Context().Err()
				}}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			s.matcher = newVideoMatcher(ctx)
			s.matcher.queue <- videoMatchAttempt{id: mustResourceID(message.ID), enqueued: time.Now().Add(-50 * time.Millisecond)}
			go s.runVideoMatcher(s.matcher)
			defer func() { cancel(); <-s.matcher.done }()
			if held != nil {
				select {
				case <-held:
				case <-time.After(time.Second):
					t.Fatal("attempt did not reach blocked media")
				}
			}
			select {
			case record := <-records:
				if record["msg"] != "automatic video attempt" || record["outcome"] != scenario || record["queueWaitMs"].(float64) < 50 || record["durationMs"].(float64) < 0 {
					t.Fatalf("wrong attempt diagnostic: %v", record)
				}
				data, _ := json.Marshal(record)
				if strings.Contains(string(data), "sensitive-payload-marker") {
					t.Fatal("attempt diagnostic exposed message text")
				}
			case <-time.After(videoMatchTimeout + time.Second):
				t.Fatal("attempt diagnostic missing")
			}
			noAutomaticVideo(t, sub)
		})
	}
}
