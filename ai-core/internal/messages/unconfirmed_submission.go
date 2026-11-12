package messages

import "fmt"

// UnconfirmedSubmission identifies an IRC write whose channel visibility is unconfirmed.
type UnconfirmedSubmission struct {
	ID         string
	RequestKey string
}

func (s *UnconfirmedSubmission) Error() string {
	return fmt.Sprintf("submitted; visibility unconfirmed id=%s requestKey=%s", s.ID, s.RequestKey)
}
