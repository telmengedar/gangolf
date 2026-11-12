package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"strings"
	"time"
)

func (c *twitchClient) dialIRC(ctx context.Context) (net.Conn, error) {
	address, err := url.Parse(c.ircURL)
	if err != nil || address.Scheme != "tls" || address.User != nil || address.Path != "" || address.RawQuery != "" || address.Fragment != "" {
		return nil, errTwitchTransport
	}
	local := address.Hostname() == "127.0.0.1" || address.Hostname() == "localhost" || address.Hostname() == "::1"
	if address.Host != "irc.chat.twitch.tv:6697" && !local {
		return nil, errTwitchTransport
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if local && c.ircTLS != nil {
		config = c.ircTLS.Clone()
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
	socket, err := dialer.DialContext(ctx, "tcp", address.Host)
	if err != nil {
		return nil, errTwitchTransport
	}
	return socket, nil
}

func (r *twitchRuntime) ircConnection(ctx context.Context, c *twitchClient, p twitchProfile, queue chan<- twitchEvent, update func(twitchProfile, string, string, [32]byte)) error {
	credential, err := c.authorize(ctx, p)
	if err != nil {
		return err
	}
	socket, err := c.dialIRC(ctx)
	if err != nil {
		return err
	}
	session := &twitchIRCSession{socket: socket, profile: p, fingerprint: credential.fingerprint()}
	r.mu.Lock()
	if r.irc == nil {
		r.irc = map[int64]*twitchIRCSession{}
	}
	r.irc[p.id] = session
	r.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	defer func() {
		socket.Close()
		session.mu.Lock()
		session.ready = false
		session.mu.Unlock()
		r.mu.Lock()
		if r.irc[p.id] == session {
			delete(r.irc, p.id)
		}
		r.mu.Unlock()
	}()
	if _, err := session.write(ctx, "PASS oauth:"+credential.AccessToken+"\r\nNICK "+credential.Login+"\r\nCAP REQ :twitch.tv/commands twitch.tv/tags twitch.tv/membership\r\nJOIN #"+p.ChannelLogin+"\r\n"); err != nil {
		return errTwitchTransport
	}
	deadline := time.Now().Add(15 * time.Second)
	validationAt := credential.revalidateAt
	reader := bufio.NewReaderSize(socket, 8192)
	authenticated, identity, joined, room := false, false, false, false
	capabilities := map[string]bool{}
	ready := false
	for {
		if !time.Now().Before(validationAt) {
			if err := c.validateCredential(ctx, p, &credential, "chat:read"); err != nil {
				return err
			}
			validationAt = credential.revalidateAt
			update(p, "connected", credential.diagnostic, session.fingerprint)
		}
		next := minTime(validationAt, time.Now().Add(time.Minute))
		if !ready {
			next = minTime(next, deadline)
		}
		socket.SetReadDeadline(next)
		line, err := reader.ReadSlice('\n')
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() && len(line) == 0 && ready && ctx.Err() == nil {
				continue
			}
			return errTwitchTransport
		}
		if len(line) > 8192 || !strings.HasSuffix(string(line), "\r\n") {
			return errTwitchProtocol
		}
		f, ok := parseIRCFrame(strings.TrimSuffix(string(line), "\r\n"))
		if !ok {
			continue
		}
		switch f.command {
		case "PING":
			if len(f.params) != 1 || strings.ContainsAny(f.params[0], "\r\n\x00") {
				return errTwitchProtocol
			}
			session.mu.Lock()
			_, err := session.write(ctx, "PONG :"+f.params[0]+"\r\n")
			session.mu.Unlock()
			if err != nil {
				return errTwitchTransport
			}
		case "001":
			if len(f.params) < 1 || f.params[0] != credential.Login {
				return errTwitchAuth
			}
			authenticated = true
		case "CAP":
			if len(f.params) != 3 || f.params[1] != "ACK" {
				return errTwitchProtocol
			}
			for _, cap := range strings.Fields(f.params[2]) {
				capabilities[cap] = true
			}
		case "GLOBALUSERSTATE":
			if f.tags["user-id"] != p.BotUserID {
				return errTwitchForbidden
			}
			identity = true
		case "JOIN":
			login, _, _ := strings.Cut(f.prefix, "!")
			if login == credential.Login && len(f.params) == 1 && f.params[0] == "#"+p.ChannelLogin {
				joined = true
			}
		case "ROOMSTATE":
			if len(f.params) == 1 && f.params[0] == "#"+p.ChannelLogin {
				if f.tags["room-id"] != p.BroadcasterUserID {
					return errTwitchForbidden
				}
				room = true
			}
		case "NOTICE", "464":
			return errTwitchForbidden
		case "RECONNECT":
			return errTwitchTransport
		case "PRIVMSG":
			if ready {
				if e, ok := ircEvent(f, p); ok {
					select {
					case queue <- e:
					default:
						update(p, "degraded", "inbound_queue_full", session.fingerprint)
						return errTwitchOverflow
					}
				}
			}
		}
		if !ready && authenticated && identity && joined && room && capabilities["twitch.tv/commands"] && capabilities["twitch.tv/tags"] && capabilities["twitch.tv/membership"] {
			session.mu.Lock()
			session.ready, ready = true, true
			session.mu.Unlock()
			update(p, "connected", credential.diagnostic, session.fingerprint)
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
