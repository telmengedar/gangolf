package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

const (
	eventStreams      = 16
	eventUserStreams  = 4
	eventQueue        = 64
	eventBytes        = 8 << 20
	eventHeartbeat    = 15 * time.Second
	eventReadTimeout  = 10 * time.Second
	eventWriteTimeout = 5 * time.Second
)

var eventHubInit sync.Mutex

type eventEnvelope struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	Time          time.Time `json:"time"`
	Service       string    `json:"service"`
	Channel       string    `json:"channel"`
	ChannelID     *string   `json:"channelId"`
	Data          any       `json:"data"`
}

type eventFrame struct {
	envelope eventEnvelope
	bytes    []byte
}

type eventSubscription struct {
	user    string
	filter  eventFilter
	queue   chan eventFrame
	done    chan struct{}
	bytes   int
	invalid bool
	cancel  context.CancelFunc
}

type eventHub struct {
	mu       sync.Mutex
	epoch    string
	sequence uint64
	closed   bool
	subs     map[*eventSubscription]struct{}
}

func (s *server) eventHub() *eventHub {
	eventHubInit.Lock()
	defer eventHubInit.Unlock()
	if s.events == nil {
		var epoch [16]byte
		_, err := rand.Read(epoch[:])
		s.events = &eventHub{epoch: hex.EncodeToString(epoch[:]), closed: err != nil, subs: make(map[*eventSubscription]struct{})}
	}
	return s.events
}

func (h *eventHub) subscribe(user string, filter eventFilter, cancel context.CancelFunc) (*eventSubscription, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, 503
	}
	count := 0
	for sub := range h.subs {
		if sub.user == user {
			count++
		}
	}
	if len(h.subs) >= eventStreams || user != "" && count >= eventUserStreams {
		return nil, 429
	}
	sub := &eventSubscription{user: user, filter: filter, queue: make(chan eventFrame, eventQueue), done: make(chan struct{}), cancel: cancel}
	h.subs[sub] = struct{}{}
	return sub, 0
}

func (h *eventHub) bind(sub *eventSubscription, user string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for other := range h.subs {
		if other != sub && other.user == user {
			count++
		}
	}
	if sub.invalid || count >= eventUserStreams {
		return false
	}
	sub.user = user
	return true
}

func (h *eventHub) removeLocked(sub *eventSubscription) {
	if sub.invalid {
		return
	}
	sub.invalid = true
	delete(h.subs, sub)
	close(sub.done)
	if sub.cancel != nil {
		sub.cancel()
	}
	for {
		select {
		case <-sub.queue:
		default:
			sub.bytes = 0
			return
		}
	}
}

func (h *eventHub) remove(sub *eventSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(sub)
}

func (h *eventHub) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		h.removeLocked(sub)
	}
}

func (h *eventHub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subs {
		h.removeLocked(sub)
	}
}

func (h *eventHub) consume(sub *eventSubscription, frame eventFrame) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sub.invalid {
		return false
	}
	sub.bytes -= len(frame.bytes)
	return true
}

func (h *eventHub) active(sub *eventSubscription) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !sub.invalid
}

func (s *server) publishEvent(kind, service, channel string, channelID *string, data any) eventEnvelope {
	h := s.eventHub()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	e := eventEnvelope{SchemaVersion: 1, ID: h.epoch + ":" + strconv.FormatUint(h.sequence, 10), Type: kind, Time: s.now().UTC(), Service: service, Channel: channel, ChannelID: channelID, Data: data}
	payload, _ := json.Marshal(e)
	frame := eventFrame{envelope: e, bytes: append(append([]byte("event: "+kind+"\ndata: "), payload...), '\n', '\n')}
	for sub := range h.subs {
		if !sub.filter.matches(e) {
			continue
		}
		if len(frame.bytes) > eventBytes || sub.bytes+len(frame.bytes) > eventBytes || len(sub.queue) == eventQueue {
			slog.Warn("event subscriber overflow", "reason", "queue_full", "queued", len(sub.queue), "queuedBytes", sub.bytes)
			h.removeLocked(sub)
			continue
		}
		sub.bytes += len(frame.bytes)
		sub.queue <- frame
	}
	return e
}

func (s *server) createdMessage(id int64, m messageContent) {
	s.publishEvent("message.created", m.Service, m.Channel, &m.ChannelID, struct {
		ResourceVersion int             `json:"resourceVersion"`
		Message         messageResource `json:"message"`
	}{3, m.resource(id)})
}

func (s *server) createdLegacy(batch int64, index int, m message) {
	s.publishEvent("message.created", m.Service, m.Channel, nil, struct {
		ResourceVersion int     `json:"resourceVersion"`
		BatchID         string  `json:"batchId"`
		Index           int     `json:"index"`
		Message         message `json:"message"`
	}{2, strconv.FormatInt(batch, 10), index, m})
}
