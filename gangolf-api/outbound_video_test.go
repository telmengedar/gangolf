package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func configureOutboundVideo(t *testing.T, s *server) (*mediaFixture, *eventSubscription) {
	t.Helper()
	media, _, carrier := newMediaFixture(t)
	s.media = carrier.media
	catalog := media.state(t)
	catalog.Clips = []mediaClip{{ID: "1", Provider: "youtube", RemoteVideoID: "abcdefghijk"}}
	catalog.Triggers = []mediaTrigger{{ID: "1", Text: "grüße", ClipID: new("1")}}
	media.set(catalog)
	s.matcher = newVideoMatcher(context.Background())
	sub, code := s.eventHub().subscribe("outbound-video", eventFilter{types: map[string]bool{"video.triggered": true}}, nil)
	if code != 0 {
		t.Fatal(code)
	}
	t.Cleanup(func() { s.eventHub().remove(sub) })
	return media, sub
}

func TestMatchMessageVideo_CompletedOutbound_PublishesOnceWithoutBotEcho(t *testing.T) {
	for _, transport := range []string{"helix", "irc"} {
		t.Run(transport, func(t *testing.T) {
			var f *resourceFixture
			var s *server
			var tw *twitchFake
			var fake *twitchIRCFake
			state := "sent"
			if transport == "irc" {
				var profile twitchProfile
				f, s, tw, profile = ircProfileFixture(t)
				fake = newIRCFake(t, tw.client, ircWelcome)
				startIRCFixture(t, s, profile)
				waitIRCReady(t, s, profile)
				state = "submitted"
			} else {
				f, s, tw = outboundFixture(t)
				tw.send = func(w http.ResponseWriter, r *http.Request) {
					if len(s.matcher.queue) != 0 {
						t.Error("reservation scheduled video before send")
					}
					respond(w, 200, map[string]any{"data": []map[string]any{{"message_id": "outbound-video", "is_sent": true}}})
				}
			}
			_, sub := configureOutboundVideo(t, s)
			response := outboundRequest(s, "key.secret", "outbound-video", outboundBody)
			status(t, response, 201)
			message := outboundResource(t, response, state)
			if fake != nil {
				if !strings.HasPrefix(takeIRCFrame(t, fake), "PRIVMSG ") {
					t.Fatal("successful IRC write not reached")
				}
			}
			if len(s.matcher.queue) != 1 {
				t.Fatal("durable success did not schedule once")
			}
			attempt := <-s.matcher.queue
			if attempt.id != mustResourceID(message.ID) || attempt.enqueued.IsZero() {
				t.Fatal("wrong queued canonical identity")
			}
			if s.matchMessageVideo(context.Background(), attempt.id, func(int) int { return 0 }, nil) != "source_unauthorized" {
				t.Fatal("outbound history without original send identity was accepted")
			}
			noAutomaticVideo(t, sub)
			ctx, cancel := context.WithCancel(context.Background())
			s.matcher = newVideoMatcher(ctx)
			s.matcher.choose = func(int) int { return 0 }
			s.matcher.queue <- attempt
			go s.runVideoMatcher(s.matcher)
			t.Cleanup(func() { cancel(); <-s.matcher.done })
			expectAutomaticVideo(t, sub, message.ID)
			status(t, outboundRequest(s, "key.secret", "outbound-video", outboundBody), 200)
			if len(s.matcher.queue) != 0 {
				t.Fatal("replay scheduled another attempt")
			}
			cfg, err := s.graph.loadConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			echo := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "900", ChatterUserLogin: "bot", ChatterUserName: "Bot", MessageID: "echo-video", publishedAt: s.now()}
			echo.Message.Text = "grüße"
			id, err := s.ingestTwitch(context.Background(), cfg.twitch[0], echo)
			if err != nil {
				t.Fatal(err)
			}
			if result := s.matchMessageVideo(context.Background(), id, func(int) int { return 0 }, nil); result != "source_unauthorized" {
				t.Fatalf("bot echo result=%s", result)
			}
			noAutomaticVideo(t, sub)
			if len(s.matcher.queue) != 0 || storedOutbound(t, f, message.ID).Delivery.State != state {
				t.Fatal("echo changed outbound identity or scheduled video")
			}
		})
	}
}

func TestCreateTwitchOutbound_ReceiverChangesDuringSend_RetainsOriginalVideoIdentity(t *testing.T) {
	f, s, tw := outboundFixture(t)
	_, sub := configureOutboundVideo(t, s)
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		changeProfile(f, 30, func(p *twitchProfile) { p.BotUserID = "901" })
		respond(w, 200, map[string]any{"data": []map[string]any{{"message_id": "original-identity", "is_sent": true}}})
	}
	response := outboundRequest(s, "key.secret", "original-identity", outboundBody)
	status(t, response, 201)
	message := outboundResource(t, response, "sent")
	if sends(tw) != 1 || len(s.matcher.queue) != 1 {
		t.Fatal("send transition scenario was not reached")
	}
	attempt := <-s.matcher.queue
	if attempt.id != mustResourceID(message.ID) || attempt.profile == nil || attempt.profile.BotUserID != "900" {
		t.Fatal("attempt captured a replacement identity after the send")
	}
	if s.matchMessageVideo(context.Background(), attempt.id, func(int) int { return 0 }, attempt.profile) != "source_unauthorized" {
		t.Fatal("changed receiver after send was allowed")
	}
	noAutomaticVideo(t, sub)
}

func TestMatchMessageVideo_OriginalActorOrSourceRevoked_SuppressesOutbound(t *testing.T) {
	for _, scenario := range []string{"actor-disabled", "actor-removed", "actor-grant", "profile-disabled", "profile-tenant", "profile-bot", "profile-client", "profile-credential", "channel-deleted", "canonical-name", "delivery-actor", "delivery-key", "author-marker", "unknown", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host", "admin"}}
			actor := addTestKey(f, s, "actor", "8", "admin")
			_, sub := configureOutboundVideo(t, s)
			tw.send = func(w http.ResponseWriter, r *http.Request) {
				respond(w, 200, map[string]any{"data": []map[string]any{{"message_id": "actor-video", "is_sent": true}}})
			}
			for _, key := range []string{"positive", "revoked"} {
				response := outboundRequest(s, actor, key, outboundBody)
				status(t, response, 201)
				message := outboundResource(t, response, "sent")
				if len(s.matcher.queue) != 1 {
					t.Fatal("successful source was not queued")
				}
				attempt := <-s.matcher.queue
				if key == "positive" {
					if s.matchMessageVideo(context.Background(), attempt.id, func(int) int { return 0 }, attempt.profile) != "published" {
						t.Fatal("current original actor positive denied")
					}
					expectAutomaticVideo(t, sub, message.ID)
					continue
				}
				held, release := make(chan struct{}), make(chan struct{})
				var entered, released sync.Once
				original := s.media.graph.http
				s.media.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
					entered.Do(func() { close(held) })
					select {
					case <-release:
						return nil
					case <-r.Context().Done():
						return r.Context().Err()
					}
				}}}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				defer released.Do(func() { close(release) })
				result := make(chan string, 1)
				go func() { result <- s.matchMessageVideo(ctx, attempt.id, func(int) int { return 0 }, attempt.profile) }()
				select {
				case <-held:
				case <-ctx.Done():
					t.Fatal("outbound media preflight did not block")
				}
				switch scenario {
				case "actor-disabled", "actor-grant", "actor-removed":
					f.legacy.mu.Lock()
					user := f.legacy.cfg.Users["8"]
					if scenario == "actor-disabled" {
						user.Enabled = false
					} else {
						user.Permissions = []string{"read"}
					}
					f.legacy.cfg.Users["8"] = user
					if scenario == "actor-removed" {
						delete(f.legacy.cfg.Users, "8")
					}
					f.legacy.mu.Unlock()
				case "profile-disabled":
					changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
				case "profile-tenant":
					changeProfile(f, 30, func(p *twitchProfile) { p.TenantUserID = "8" })
				case "profile-bot":
					changeProfile(f, 30, func(p *twitchProfile) { p.BotUserID = "901" })
				case "profile-client":
					changeProfile(f, 30, func(p *twitchProfile) { p.ClientID = "other-client" })
				case "profile-credential":
					changeProfile(f, 30, func(p *twitchProfile) { p.CredentialRef = "other-grant" })
				case "channel-deleted":
					f.mu.Lock()
					delete(f.nodes, 31)
					f.mu.Unlock()
				default:
					f.mu.Lock()
					node := f.nodes[attempt.id]
					var stored messageContent
					json.Unmarshal([]byte(*node.Content), &stored)
					switch scenario {
					case "canonical-name":
						node.Name = outboundName("12", "8", "other-key")
					case "delivery-actor":
						stored.Delivery.AuthUserID = "7"
					case "delivery-key":
						stored.Delivery.RequestKey = "other-key"
					case "author-marker":
						stored.User.ServiceID = "900"
					case "unknown", "failed":
						stored.Delivery.State = scenario
						stored.Delivery.MessageID = ""
						if scenario == "failed" {
							stored.Delivery.Code = "twitch_rejected"
						}
					}
					data, _ := json.Marshal(stored)
					node.Content = new(string(data))
					f.nodes[attempt.id] = node
					f.mu.Unlock()
				}
				released.Do(func() { close(release) })
				select {
				case outcome := <-result:
					if outcome == "published" {
						t.Fatal("changed source or original actor still published")
					}
				case <-ctx.Done():
					t.Fatal("outbound media preflight did not resume")
				}
				noAutomaticVideo(t, sub)
			}
		})
	}
}

func TestCreateTwitchOutbound_FailedUnknownOrMediaUnavailable_PreservesChatOutcome(t *testing.T) {
	for _, scenario := range []string{"failed", "unknown", "media-unavailable", "canceled-request"} {
		t.Run(scenario, func(t *testing.T) {
			_, s, tw := outboundFixture(t)
			media, sub := configureOutboundVideo(t, s)
			tw.send = func(w http.ResponseWriter, r *http.Request) {
				if len(s.matcher.queue) != 0 {
					t.Error("reservation was scheduled")
				}
				switch scenario {
				case "failed":
					respond(w, 403, map[string]any{})
				case "unknown":
					respond(w, 200, map[string]any{})
				default:
					respond(w, 200, map[string]any{"data": []map[string]any{{"message_id": "media-independent", "is_sent": true}}})
				}
			}
			if scenario == "media-unavailable" {
				media.fail = "all"
			}
			request, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			r := httptest.NewRequest("POST", "/api/v3/messages", strings.NewReader(outboundBody)).WithContext(request)
			r.Header.Set("Authorization", "Bearer key.secret")
			r.Header.Set("Idempotency-Key", "failure-video")
			response := httptest.NewRecorder()
			s.ServeHTTP(response, r)
			if scenario == "failed" || scenario == "unknown" {
				code := 502
				if scenario == "unknown" {
					code = 503
				}
				status(t, response, code)
				if sends(tw) != 1 || len(s.matcher.queue) != 0 {
					t.Fatal("failed or unknown outcome scheduled video")
				}
				return
			}
			status(t, response, 201)
			message := outboundResource(t, response, "sent")
			if len(s.matcher.queue) != 1 {
				t.Fatal("durable chat success was not retained")
			}
			attempt := <-s.matcher.queue
			if scenario == "media-unavailable" {
				if result := s.matchMessageVideo(context.Background(), attempt.id, func(int) int { return 0 }, attempt.profile); result != "media_unavailable" {
					t.Fatalf("outage scenario missing: %s", result)
				}
				noAutomaticVideo(t, sub)
			} else {
				cancelRequest()
				if request.Err() == nil {
					t.Fatal("request cancellation not reached")
				}
				ctx, cancelWorker := context.WithCancel(context.Background())
				s.matcher = newVideoMatcher(ctx)
				s.matcher.queue <- attempt
				go s.runVideoMatcher(s.matcher)
				expectAutomaticVideo(t, sub, message.ID)
				cancelWorker()
				select {
				case <-s.matcher.done:
				case <-time.After(time.Second):
					t.Fatal("worker did not finish")
				}
			}
		})
	}
}
