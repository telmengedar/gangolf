package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

type credentialTransport struct {
	token string
	query bool
}

var credentialContextKey = &struct{ name string }{"credential"}

func credential(r *http.Request) (string, bool) {
	if value, ok := r.Context().Value(credentialContextKey).(credentialTransport); ok {
		return value.token, value.query
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 8192 {
		return "", false
	}
	return extractCredential(r, q)
}

func prepareCredential(r *http.Request) (*http.Request, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 8192 {
		return r, false
	}
	token, query := extractCredential(r, q)
	_, header := r.Header["Authorization"]
	if (header || q.Has("apiKey")) && token == "" {
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), credentialContextKey, credentialTransport{token: token, query: query})), true
}

func extractCredential(r *http.Request, q url.Values) (string, bool) {
	headers, header := r.Header["Authorization"]
	values, query := q["apiKey"]
	if header && query {
		return "", false
	}
	var token string
	if header {
		if len(headers) != 1 {
			return "", false
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", false
		}
		token = parts[1]
	} else if query {
		if len(values) != 1 || r.Method != http.MethodGet {
			return "", true
		}
		token = values[0]
	}
	if len(token) == 0 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n,") {
		return "", query
	}
	return token, query
}
