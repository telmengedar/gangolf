package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"
)

const captureLimit = 2 << 20

type processExchange struct {
	Request           json.RawMessage `json:"request,omitempty"`
	RequestSHA256     string          `json:"requestSHA256,omitempty"`
	Status            int             `json:"status,omitempty"`
	Response          json.RawMessage `json:"response,omitempty"`
	ResponseSHA256    string          `json:"responseSHA256,omitempty"`
	MalformedResponse []byte          `json:"malformedResponse,omitempty"`
	CaptureError      string          `json:"captureError,omitempty"`
}

type captureTransport struct {
	base      http.RoundTripper
	exchanges []processExchange
}

func captureHash(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

func (c *captureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	record := processExchange{}
	defer func() { c.exchanges = append(c.exchanges, record) }()
	if request.GetBody == nil {
		record.CaptureError = "request_capture"
		return nil, fmt.Errorf("private request capture unavailable")
	}
	duplicate, err := request.GetBody()
	if err != nil {
		record.CaptureError = "request_capture"
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(duplicate, captureLimit+1))
	duplicate.Close()
	if err != nil {
		record.CaptureError = "request_read"
		return nil, err
	}
	if len(body) > captureLimit {
		record.CaptureError = "request_limit"
		return nil, fmt.Errorf("private request capture bound exceeded")
	}
	record.Request = body
	record.RequestSHA256 = captureHash(body)
	response, err := c.base.RoundTrip(request)
	if err != nil {
		record.CaptureError = "transport"
		return nil, err
	}
	record.Status = response.StatusCode
	body, err = io.ReadAll(io.LimitReader(response.Body, captureLimit+1))
	response.Body.Close()
	if err != nil {
		record.CaptureError = "response_read"
		return nil, err
	}
	if len(body) > captureLimit {
		record.CaptureError = "response_limit"
		return nil, fmt.Errorf("private response capture bound exceeded")
	}
	record.ResponseSHA256 = captureHash(body)
	if utf8.Valid(body) && json.Valid(body) {
		record.Response = body
	} else {
		record.MalformedResponse = body
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
