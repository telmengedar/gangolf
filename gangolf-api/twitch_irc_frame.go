package main

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type twitchIRCFrame struct {
	tags    map[string]string
	prefix  string
	command string
	params  []string
}

func parseIRCFrame(line string) (twitchIRCFrame, bool) {
	f := twitchIRCFrame{tags: map[string]string{}}
	if len(line) > 8192 || !utf8.ValidString(line) || strings.ContainsAny(line, "\r\n\x00") {
		return f, false
	}
	if strings.HasPrefix(line, "@") {
		tags, rest, ok := strings.Cut(line[1:], " ")
		if !ok {
			return f, false
		}
		for _, tag := range strings.Split(tags, ";") {
			key, value, _ := strings.Cut(tag, "=")
			if key == "" {
				return f, false
			}
			if _, exists := f.tags[key]; exists {
				return f, false
			}
			f.tags[key] = unescapeIRCTag(value)
		}
		line = rest
	}
	if strings.HasPrefix(line, ":") {
		var ok bool
		f.prefix, line, ok = strings.Cut(line[1:], " ")
		if !ok || f.prefix == "" {
			return f, false
		}
	}
	line = strings.TrimLeft(line, " ")
	f.command, line, _ = strings.Cut(line, " ")
	if f.command == "" {
		return f, false
	}
	for line != "" {
		line = strings.TrimLeft(line, " ")
		if strings.HasPrefix(line, ":") {
			f.params = append(f.params, line[1:])
			break
		}
		value, rest, _ := strings.Cut(line, " ")
		if value != "" {
			f.params = append(f.params, value)
		}
		line = rest
		if len(f.params) > 15 {
			return f, false
		}
	}
	return f, true
}

func unescapeIRCTag(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			continue
		}
		i++
		if i == len(value) {
			break
		}
		switch value[i] {
		case 's':
			out.WriteByte(' ')
		case ':':
			out.WriteByte(';')
		case 'r':
			out.WriteByte('\r')
		case 'n':
			out.WriteByte('\n')
		default:
			out.WriteByte(value[i])
		}
	}
	return out.String()
}

func ircEvent(f twitchIRCFrame, p twitchProfile) (twitchEvent, bool) {
	var e twitchEvent
	login, _, hasUser := strings.Cut(f.prefix, "!")
	stamp, err := strconv.ParseInt(f.tags["tmi-sent-ts"], 10, 64)
	if f.command != "PRIVMSG" || len(f.params) != 2 || f.params[0] != "#"+p.ChannelLogin || f.tags["room-id"] != p.BroadcasterUserID || !positiveDecimal(f.tags["user-id"]) || !safeMessageID(f.tags["id"]) || !hasUser || !validTwitchLogin(login) || err != nil || stamp <= 0 || stamp > 253402300799999 || f.params[1] == "" {
		return e, false
	}
	e.BroadcasterUserID, e.BroadcasterUserLogin = p.BroadcasterUserID, p.ChannelLogin
	e.ChatterUserID, e.ChatterUserLogin, e.ChatterUserName = f.tags["user-id"], login, f.tags["display-name"]
	if e.ChatterUserName == "" {
		e.ChatterUserName = login
	}
	e.MessageID, e.Message.Text, e.publishedAt = f.tags["id"], f.params[1], time.UnixMilli(stamp).UTC()
	if parent := f.tags["reply-parent-msg-id"]; parent != "" {
		if !safeMessageID(parent) {
			return e, false
		}
		e.Reply = &struct {
			ParentMessageID string `json:"parent_message_id"`
		}{parent}
	}
	return e, true
}
