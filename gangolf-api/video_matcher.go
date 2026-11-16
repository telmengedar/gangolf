package main

import (
	"context"
	"log"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"
)

const videoMatchQueue = 64
const videoMatchTimeout = 3 * time.Second

type videoMatcher struct {
	queue   chan int64
	done    chan struct{}
	context context.Context
	choose  func(int) int
	dropped atomic.Uint64
}

func newVideoMatcher(ctx context.Context) *videoMatcher {
	return &videoMatcher{queue: make(chan int64, videoMatchQueue), done: make(chan struct{}), context: ctx, choose: rand.IntN}
}

func (s *server) scheduleVideo(id int64) {
	m := s.matcher
	if m == nil || m.context.Err() != nil {
		return
	}
	select {
	case m.queue <- id:
	default:
		if m.dropped.Add(1) == 1 {
			log.Print("gangolf-api: automatic video queue full")
		}
	}
}

func (s *server) runVideoMatcher(m *videoMatcher) {
	defer close(m.done)
	for m.context.Err() == nil {
		select {
		case <-m.context.Done():
			return
		case id := <-m.queue:
			ctx, cancel := context.WithTimeout(m.context, videoMatchTimeout)
			s.matchMessageVideo(ctx, id, m.choose)
			cancel()
		}
	}
}

func (s *server) matchMessageVideo(ctx context.Context, id int64, choose func(int) int) {
	media := s.media
	if media == nil || lockMutex(ctx, &media.mu) != nil {
		return
	}
	catalog, err := media.load(ctx)
	generation := media.generation
	media.mu.Unlock()
	if err != nil || lockResources(ctx) != nil {
		return
	}
	defer resourceLock.Unlock()
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return
	}
	user, ok := cfg.Users[media.tenant]
	if !ok || !user.Enabled || !hasHost(user.Permissions) {
		return
	}
	source, found, err := s.videoSource(ctx, cfg, id)
	if err != nil || !found || source.Delivery != nil {
		return
	}
	tenant, status := s.boundPrincipal(ctx, cfg, principal{UserID: media.tenant, Permissions: []string{"host"}, ManagedOnly: user.ManagedOnly})
	if status != 0 {
		return
	}
	policy := s.policy(tenant, cfg)
	allowed, err := policy.scopedReadChannel(ctx, source.ChannelID, true)
	if err != nil || !allowed {
		return
	}
	if source.Source != nil {
		eligible := false
		for _, profile := range cfg.twitch {
			if source.Source.ConfigID == strconv.FormatInt(profile.id, 10) && profile.TenantUserID == media.tenant && profile.Enabled != nil && *profile.Enabled && source.User.ServiceID != profile.BotUserID {
				eligible = true
			}
		}
		if !eligible {
			return
		}
	}
	trigger, clip, matched := matchVideo(catalog, messageVideoText(source), choose)
	if !matched || ctx.Err() != nil || !media.mu.TryLock() {
		return
	}
	defer media.mu.Unlock()
	if media.generation != generation || ctx.Err() != nil {
		return
	}
	s.publishVideo(strconv.FormatInt(id, 10), source, trigger, clip)
}
