package main

import (
	"encoding/json"
	"strconv"
	"time"
)

type messageInput struct {
	Service string          `json:"service"`
	Channel string          `json:"channel"`
	User    author          `json:"user"`
	Chunks  []chunk         `json:"chunks"`
	TTS     bool            `json:"tts"`
	ReplyTo *string         `json:"replyTo"`
	PostTo  json.RawMessage `json:"postTo,omitempty"`
}

func (m messageInput) snapshot() message {
	return message{Service: m.Service, Channel: m.Channel, User: m.User, Chunks: m.Chunks, TTS: m.TTS}
}

type messageResource struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	ChannelID string    `json:"channelId"`
	ReplyTo   *string   `json:"replyTo"`
	Time      time.Time `json:"time"`
	Service   string    `json:"service"`
	Channel   string    `json:"channel"`
	User      author    `json:"user"`
	Chunks    []chunk   `json:"chunks"`
	TTS       bool      `json:"tts"`
}

type messageContent struct {
	SchemaVersion int       `json:"schemaVersion"`
	Deleted       bool      `json:"deleted,omitempty"`
	UserID        string    `json:"userId"`
	ChannelID     string    `json:"channelId"`
	ReplyTo       *string   `json:"replyTo"`
	Time          time.Time `json:"time"`
	Service       string    `json:"service,omitempty"`
	Channel       string    `json:"channel,omitempty"`
	User          *author   `json:"user,omitempty"`
	Chunks        []chunk   `json:"chunks,omitempty"`
	TTS           *bool     `json:"tts,omitempty"`
}

func (m messageContent) resource(id int64) messageResource {
	return messageResource{ID: strconv.FormatInt(id, 10), UserID: m.UserID, ChannelID: m.ChannelID, ReplyTo: m.ReplyTo, Time: m.Time, Service: m.Service, Channel: m.Channel, User: *m.User, Chunks: m.Chunks, TTS: *m.TTS}
}

func resourceID(id string) (int64, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	return n, err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

type identityContent struct {
	SchemaVersion int    `json:"schemaVersion"`
	Service       string `json:"service"`
	ServiceID     string `json:"serviceId,omitempty"`
	Name          string `json:"name,omitempty"`
	Channel       string `json:"channel,omitempty"`
}

type relation struct {
	Source  int64  `json:"sourceId"`
	Target  int64  `json:"targetId"`
	Type    string `json:"linkType"`
	Context string `json:"context"`
}

type patchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}
