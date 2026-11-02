package main

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func (c *twitchClient) reconnectAddress(raw string) bool {
	address, err := url.Parse(raw)
	original, originalErr := url.Parse(c.socketURL)
	if err != nil || originalErr != nil || address.User != nil || address.Fragment != "" {
		return false
	}
	return address.Scheme == "wss" && address.Host == "eventsub.wss.twitch.tv" || original.Scheme == "ws" && address.Scheme == "ws" && address.Host == original.Host && (address.Hostname() == "127.0.0.1" || address.Hostname() == "localhost" || address.Hostname() == "::1")
}

func (c *twitchClient) welcome(ctx context.Context, address string) (*websocket.Conn, twitchEnvelope, time.Time, error) {
	var welcome twitchEnvelope
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	socket, response, err := dialer.DialContext(ctx, address, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return nil, welcome, time.Time{}, errTwitchTransport
	}
	socket.SetReadLimit(1 << 20)
	socket.SetReadDeadline(time.Now().Add(10 * time.Second))
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	kind, data, err := socket.ReadMessage()
	receivedAt := time.Now()
	if err != nil {
		socket.Close()
		return nil, welcome, time.Time{}, errTwitchTransport
	}
	if kind != websocket.TextMessage || json.Unmarshal(data, &welcome) != nil || !validWelcome(welcome) {
		socket.Close()
		return nil, welcome, time.Time{}, errTwitchProtocol
	}
	return socket, welcome, receivedAt, nil
}

func validWelcome(e twitchEnvelope) bool {
	session := e.Payload.Session
	return validEnvelope(e) && e.Metadata.MessageType == "session_welcome" && session.ID != "" && session.Status == "connected" && session.KeepaliveTimeoutSeconds >= 1 && session.KeepaliveTimeoutSeconds <= 600
}

func validEnvelope(e twitchEnvelope) bool {
	timestamp, err := time.Parse(time.RFC3339Nano, e.Metadata.MessageTimestamp)
	return e.Metadata.MessageID != "" && err == nil && !timestamp.IsZero()
}

func readTwitchFrames(ctx context.Context, socket *websocket.Conn, welcome twitchEnvelope, frames chan<- twitchFrame) {
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	defer socket.Close()
	session := welcome.Payload.Session.ID
	timeout := time.Duration(welcome.Payload.Session.KeepaliveTimeoutSeconds) * time.Second
	for {
		socket.SetReadDeadline(time.Now().Add(timeout))
		kind, data, err := socket.ReadMessage()
		frame := twitchFrame{connection: socket, session: session, err: err}
		if err == nil && (kind != websocket.TextMessage || json.Unmarshal(data, &frame.envelope) != nil || !validEnvelope(frame.envelope)) {
			frame.err = errTwitchProtocol
		}
		select {
		case frames <- frame:
		case <-ctx.Done():
			return
		}
		if frame.err != nil {
			return
		}
	}
}

func (c *twitchClient) connection(ctx context.Context, profiles []twitchProfile, queue chan<- twitchEvent, update func(twitchProfile, string, string, [32]byte)) error {
	p := profiles[0]
	if err := c.waitRateLimit(ctx); err != nil {
		return err
	}
	credential, err := c.authorize(ctx, p)
	if err != nil {
		return err
	}
	socket, welcome, welcomeAt, err := c.welcome(ctx, c.socketURL)
	if err != nil {
		return err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { socket.Close() }()
	frames := make(chan twitchFrame)
	go readTwitchFrames(child, socket, welcome, frames)
	welcomeDeadline := welcomeAt.Add(time.Duration(welcome.Payload.Session.KeepaliveTimeoutSeconds) * time.Second)
	subscriptionTimeout := min(10*time.Second, time.Until(welcomeDeadline))
	if subscriptionTimeout <= 0 {
		return errTwitchTransport
	}
	subscriptions := map[string]string{}
	for _, profile := range profiles {
		subscriptionCtx, finish := context.WithTimeout(ctx, subscriptionTimeout)
		id, err := c.subscribe(subscriptionCtx, profile, welcome.Payload.Session.ID, &credential)
		finish()
		if err == errTwitchForbidden {
			update(profile, "auth-required", "subscription_forbidden", credential.fingerprint())
			continue
		}
		if err != nil {
			return err
		}
		subscriptions[profile.BroadcasterUserID] = id
		update(profile, "connected", "", credential.fingerprint())
	}
	if len(subscriptions) == 0 {
		return errTwitchForbidden
	}
	validation := time.NewTicker(time.Hour)
	defer validation.Stop()
	replacement := make(chan twitchFrame, 1)
	var handovers sync.WaitGroup
	defer func() {
		cancel()
		handovers.Wait()
		select {
		case next := <-replacement:
			if next.connection != nil {
				next.connection.Close()
			}
		default:
		}
	}()
	transferring := false
	var transferID, transferSession, transferURL string
	var transferSocket *websocket.Conn
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-validation.C:
			err := c.validate(ctx, p, credential)
			if err == errTwitchAuth {
				credential, err = c.refresh(ctx, p, credential)
			}
			if err != nil {
				return err
			}
			for _, profile := range profiles {
				if _, ok := subscriptions[profile.BroadcasterUserID]; ok {
					update(profile, "connected", "", credential.fingerprint())
				}
			}
		case next := <-replacement:
			transferring = false
			if next.err != nil {
				return next.err
			}
			old := socket
			socket = next.connection
			welcome = next.envelope
			welcomeAt = next.receivedAt
			old.Close()
			go readTwitchFrames(child, socket, welcome, frames)
		case frame := <-frames:
			if frame.err != nil {
				if frame.connection != socket {
					continue
				}
				if frame.err == errTwitchProtocol {
					return frame.err
				}
				return errTwitchTransport
			}
			e := frame.envelope
			switch e.Metadata.MessageType {
			case "session_keepalive":
			case "notification":
				var profile twitchProfile
				for _, candidate := range profiles {
					if candidate.BroadcasterUserID == e.Payload.Subscription.Condition.BroadcasterUserID {
						profile = candidate
						break
					}
				}
				id, active := subscriptions[profile.BroadcasterUserID]
				if !active || e.Metadata.SubscriptionType != "channel.chat.message" || e.Metadata.SubscriptionVersion != "1" || !e.Payload.Subscription.matches(profile, frame.session, id) {
					return errTwitchProtocol
				}
				published, _ := time.Parse(time.RFC3339Nano, e.Metadata.MessageTimestamp)
				event, err := parseTwitchEvent(e.Payload.Event, published, profile)
				if err != nil {
					return errTwitchProtocol
				}
				select {
				case queue <- event:
				default:
					update(profile, "degraded", "inbound_queue_full", credential.fingerprint())
					return errTwitchOverflow
				}
			case "session_reconnect":
				address := e.Payload.Session.ReconnectURL
				if frame.connection == transferSocket && frame.session == transferSession && e.Metadata.MessageID == transferID && address == transferURL {
					continue
				}
				if transferring || frame.connection != socket || e.Payload.Session.ID != frame.session || e.Payload.Session.Status != "reconnecting" || !c.reconnectAddress(address) {
					return errTwitchProtocol
				}
				transferring = true
				transferID, transferSession, transferURL, transferSocket = e.Metadata.MessageID, frame.session, address, socket
				handovers.Add(1)
				go func() {
					defer handovers.Done()
					next, hello, receivedAt, err := c.welcome(child, address)
					result := twitchFrame{connection: next, envelope: hello, receivedAt: receivedAt, err: err}
					select {
					case replacement <- result:
					case <-child.Done():
						if next != nil {
							next.Close()
						}
					}
				}()
			case "revocation":
				sub := e.Payload.Subscription
				var profile twitchProfile
				for _, candidate := range profiles {
					if candidate.BroadcasterUserID == sub.Condition.BroadcasterUserID {
						profile = candidate
						break
					}
				}
				id, active := subscriptions[profile.BroadcasterUserID]
				if !active || sub.ID != id || sub.Type != "channel.chat.message" || sub.Version != "1" || sub.Condition.UserID != profile.BotUserID {
					return errTwitchProtocol
				}
				if sub.Status == "version_removed" {
					return errTwitchProtocol
				}
				delete(subscriptions, profile.BroadcasterUserID)
				update(profile, "auth-required", "subscription_forbidden", credential.fingerprint())
				if len(subscriptions) == 0 {
					return errTwitchForbidden
				}
			default:
				return errTwitchProtocol
			}
		}
	}
}
