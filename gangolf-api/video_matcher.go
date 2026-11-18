package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"
)

const videoMatchQueue = 64
const videoMatchTimeout = 3 * time.Second

type videoMatcher struct {
	queue   chan videoMatchAttempt
	done    chan struct{}
	context context.Context
	choose  func(int) int
	dropped atomic.Uint64
}

func newVideoMatcher(ctx context.Context) *videoMatcher {
	return &videoMatcher{queue: make(chan videoMatchAttempt, videoMatchQueue), done: make(chan struct{}), context: ctx, choose: rand.IntN}
}

func (s *server) scheduleVideo(id int64, profiles ...twitchProfile) {
	m := s.matcher
	if m == nil || m.context.Err() != nil {
		return
	}
	attempt := videoMatchAttempt{id: id, enqueued: time.Now()}
	if len(profiles) > 0 {
		profile := profiles[0]
		attempt.profile = &profile
	}
	select {
	case m.queue <- attempt:
	default:
		if m.dropped.Add(1) == 1 {
			slog.Warn("automatic video queue full", "reason", "queue_full", "queued", len(m.queue), "dropped", m.dropped.Load())
		}
	}
}

func (s *server) runVideoMatcher(m *videoMatcher) {
	defer close(m.done)
	for m.context.Err() == nil {
		select {
		case <-m.context.Done():
			return
		case attempt := <-m.queue:
			started := time.Now()
			ctx, cancel := context.WithTimeout(m.context, videoMatchTimeout)
			outcome := s.matchMessageVideo(ctx, attempt.id, m.choose, attempt.profile)
			if outcome != "published" && ctx.Err() != nil {
				outcome = "canceled"
				if m.context.Err() == nil {
					outcome = "deadline"
				}
			}
			cancel()
			slog.Debug("automatic video attempt", "messageId", attempt.id, "queueWaitMs", started.Sub(attempt.enqueued).Milliseconds(), "durationMs", time.Since(started).Milliseconds(), "outcome", outcome)
		}
	}
}

func (s *server) matchMessageVideo(ctx context.Context, id int64, choose func(int) int, original *twitchProfile) string {
	media := s.media
	if media == nil || lockMutex(ctx, &media.mu) != nil {
		return "media_unavailable"
	}
	catalog, err := media.load(ctx)
	generation := media.generation
	media.mu.Unlock()
	if err != nil {
		return "media_unavailable"
	}
	if lockResources(ctx) != nil {
		return "source_unavailable"
	}
	defer resourceLock.Unlock()
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return "source_unavailable"
	}
	user, ok := cfg.Users[media.tenant]
	if !ok || !user.Enabled || !hasHost(user.Permissions) {
		return "source_unauthorized"
	}
	source, found, err := s.videoSource(ctx, cfg, id)
	if err != nil || !found {
		return "source_unavailable"
	}
	if source.Delivery != nil && !s.outboundVideoAllowed(ctx, cfg, source, original) {
		return "source_unauthorized"
	}
	tenant, status := s.boundPrincipal(ctx, cfg, principal{UserID: media.tenant, Permissions: []string{"host"}, ManagedOnly: user.ManagedOnly})
	if status != 0 {
		return "source_unauthorized"
	}
	policy := s.policy(tenant, cfg)
	allowed, err := policy.scopedReadChannel(ctx, source.ChannelID, true)
	if err != nil || !allowed {
		return "source_unauthorized"
	}
	if source.Source != nil {
		eligible := false
		for _, profile := range cfg.twitch {
			if source.Source.ConfigID == strconv.FormatInt(profile.id, 10) && profile.TenantUserID == media.tenant && profile.Enabled != nil && *profile.Enabled && source.User.ServiceID != profile.BotUserID {
				eligible = true
			}
		}
		if !eligible {
			return "source_unauthorized"
		}
	}
	trigger, clip, matched := matchVideo(catalog, messageVideoText(source), choose)
	if !matched {
		return "no_match"
	}
	if ctx.Err() != nil {
		return "deadline"
	}
	if !media.mu.TryLock() {
		return "media_busy"
	}
	defer media.mu.Unlock()
	if media.generation != generation {
		return "snapshot_changed"
	}
	if ctx.Err() != nil {
		return "deadline"
	}
	s.publishVideo(strconv.FormatInt(id, 10), source, trigger, clip)
	return "published"
}
