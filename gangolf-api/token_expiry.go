package main

import (
	"encoding/json"
	"time"
)

type tokenExpiry struct {
	present bool
	value   *time.Time
}

func (e *tokenExpiry) UnmarshalJSON(data []byte) error {
	e.present = true
	return json.Unmarshal(data, &e.value)
}
