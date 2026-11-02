package main

import (
	"github.com/gorilla/websocket"
	"time"
)

type twitchFrame struct {
	connection *websocket.Conn
	session    string
	envelope   twitchEnvelope
	err        error
	receivedAt time.Time
}
