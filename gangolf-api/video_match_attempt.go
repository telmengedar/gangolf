package main

import "time"

type videoMatchAttempt struct {
	id       int64
	enqueued time.Time
	profile  *twitchProfile
}
