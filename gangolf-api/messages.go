package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

var timeUTC = time.UTC

type chunkType int

func (c *chunkType) UnmarshalJSON(data []byte) error {
	switch strings.ToLower(string(data)) {
	case "0", `"text"`:
		*c = 0
	case "1", `"emote"`:
		*c = 1
	default:
		return errCorrupt
	}
	return nil
}

type chunk struct {
	Type   chunkType `json:"type"`
	Data   string    `json:"data,omitempty"`
	Code   string    `json:"code,omitempty"`
	Width  *int      `json:"width,omitempty"`
	Height *int      `json:"height,omitempty"`
	Color  string    `json:"color,omitempty"`
	Weight string    `json:"weight,omitempty"`
}

type author struct {
	Service   string `json:"service"`
	ServiceID string `json:"serviceId"`
	Name      string `json:"name"`
}

type message struct {
	Service string          `json:"service"`
	Channel string          `json:"channel"`
	User    author          `json:"user"`
	Time    time.Time       `json:"time"`
	Chunks  []chunk         `json:"chunks"`
	TTS     bool            `json:"tts"`
	PostTo  json.RawMessage `json:"postTo,omitempty"`
}

type batch struct {
	SchemaVersion int       `json:"schemaVersion"`
	Messages      []message `json:"messages"`
}

type storedMessage struct {
	message
	batchID int64
	index   int
}

func (m message) valid() bool {
	for _, s := range []string{m.Service, m.Channel, m.User.Service, m.User.ServiceID, m.User.Name} {
		if strings.TrimSpace(s) == "" {
			return false
		}
	}
	if len(m.Chunks) == 0 {
		return false
	}
	for _, c := range m.Chunks {
		if c.Type != 0 && c.Type != 1 {
			return false
		}
		if c.Type == 0 && strings.TrimSpace(c.Data) == "" || c.Type == 1 && strings.TrimSpace(c.Code) == "" {
			return false
		}
		if c.Width != nil && *c.Width < 0 || c.Height != nil && *c.Height < 0 {
			return false
		}
	}
	return true
}

func nonNull(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func strictJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errCorrupt
	}
	return nil
}
