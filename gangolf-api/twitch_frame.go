package main

import "github.com/gorilla/websocket"

type twitchFrame struct {
	connection *websocket.Conn
	session    string
	envelope   twitchEnvelope
	err        error
}
