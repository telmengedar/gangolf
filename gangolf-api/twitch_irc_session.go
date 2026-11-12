package main

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

type twitchIRCSession struct {
	mu          sync.Mutex
	socket      net.Conn
	profile     twitchProfile
	fingerprint [32]byte
	ready       bool
}

func (r *twitchRuntime) ircSession(id int64) *twitchIRCSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.irc[id]
}

func (r *twitchRuntime) ircReady(p twitchProfile, c twitchCredential) bool {
	session := r.ircSession(p.id)
	if session == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.ready && sameReceiver(session.profile, p) && session.fingerprint == c.fingerprint()
}

func (s *twitchIRCSession) write(ctx context.Context, frame string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err := s.socket.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, func() { s.socket.SetWriteDeadline(time.Now()) })
	defer stop()
	return io.WriteString(s.socket, frame)
}

func (r *twitchRuntime) sendIRC(ctx context.Context, p twitchProfile, credential twitchCredential, text string) (string, string) {
	session := r.ircSession(p.id)
	if session == nil {
		return "failed", "irc_not_written"
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.ready || !sameReceiver(session.profile, p) || session.fingerprint != credential.fingerprint() {
		return "failed", "irc_not_written"
	}
	frame := "PRIVMSG #" + p.ChannelLogin + " :" + text + "\r\n"
	n, err := session.write(ctx, frame)
	if err == nil && n == len(frame) {
		return "submitted", ""
	}
	session.ready = false
	session.socket.Close()
	if n == 0 {
		return "failed", "irc_not_written"
	}
	return "unknown", ""
}

func (r *twitchRuntime) stopIRC(p twitchProfile) {
	session := r.ircSession(p.id)
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.ready = false
	session.socket.Close()
}
