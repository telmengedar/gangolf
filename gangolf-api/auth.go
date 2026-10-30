package main

import (
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type grant struct {
	Enabled     bool     `json:"enabled"`
	Permissions []string `json:"permissions"`
}

type keyGrant struct {
	UserID      string     `json:"userId"`
	Hash        []byte     `json:"hash"`
	Enabled     bool       `json:"enabled"`
	Permissions []string   `json:"permissions"`
	ExpiresAt   *time.Time `json:"expiresAt"`
}

type authConfig struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Issuer        string              `json:"issuer"`
	Audience      string              `json:"audience"`
	UserIDClaim   string              `json:"userIdClaimName"`
	JWKSURL       string              `json:"jwksUrl"`
	Users         map[string]grant    `json:"users"`
	APIKeys       map[string]keyGrant `json:"apiKeys"`
}

func (c *authConfig) valid() bool {
	u, err := url.Parse(c.JWKSURL)
	if c.SchemaVersion != 1 || c.Issuer == "" || c.Audience == "" || c.UserIDClaim == "" || c.Users == nil || c.APIKeys == nil || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return false
	}
	for _, k := range c.APIKeys {
		if len(k.Hash) != sha256.Size || k.UserID == "" {
			return false
		}
	}
	return true
}

func hasHost(permissions []string) bool {
	for _, p := range permissions {
		if p == "host" || p == "admin" {
			return true
		}
	}
	return false
}

func credential(r *http.Request) (string, bool) {
	if headers, ok := r.Header["Authorization"]; ok {
		if len(headers) != 1 {
			return "", false
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", false
		}
		return parts[1], false
	}
	if r.Method == http.MethodGet {
		return r.URL.Query().Get("apiKey"), true
	}
	return "", false
}

func (s *server) authorize(ctx context.Context, r *http.Request, c *authConfig, token string, queryKey bool) int {
	if strings.Count(token, ".") == 2 && !queryKey {
		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
			kid, ok := t.Header["kid"].(string)
			if !ok || kid == "" {
				return nil, errCorrupt
			}
			return s.jwtKey(ctx, c.JWKSURL, kid)
		}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(c.Issuer), jwt.WithAudience(c.Audience), jwt.WithExpirationRequired(), jwt.WithLeeway(2*time.Minute), jwt.WithTimeFunc(s.now), jwt.WithJSONNumber())
		if err != nil {
			if errors.Is(err, errUnavailable) {
				return http.StatusServiceUnavailable
			}
			return http.StatusUnauthorized
		}
		notBefore, _ := claims.GetNotBefore()
		expires, _ := claims.GetExpirationTime()
		if notBefore != nil && notBefore.After(expires.Time) {
			return http.StatusUnauthorized
		}
		var id string
		switch value := claims[c.UserIDClaim].(type) {
		case string:
			id = value
		case json.Number:
			id = value.String()
		default:
			return http.StatusForbidden
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return http.StatusForbidden
		}
		u, ok := c.Users[strconv.FormatInt(n, 10)]
		if !ok || !u.Enabled || !hasHost(u.Permissions) {
			return http.StatusForbidden
		}
		return 0
	}
	id, secret, ok := strings.Cut(token, ".")
	if !ok || id == "" || secret == "" {
		return http.StatusUnauthorized
	}
	k, ok := c.APIKeys[id]
	h := hmac.New(sha256.New, []byte(s.pepper))
	h.Write([]byte(token))
	if !ok || !hmac.Equal(h.Sum(nil), k.Hash) || !k.Enabled || k.ExpiresAt != nil && k.ExpiresAt.Before(s.now()) {
		return http.StatusUnauthorized
	}
	user, ok := c.Users[k.UserID]
	if !ok || !user.Enabled {
		return http.StatusUnauthorized
	}
	if !hasHost(k.Permissions) {
		return http.StatusForbidden
	}
	return 0
}

func (s *server) jwtKey(ctx context.Context, target, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errUnavailable
	}
	res, err := s.graph.http.Do(req)
	if err != nil {
		return nil, errUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errUnavailable
	}
	var set struct {
		Keys []struct{ Kty, Kid, Alg, Use, N, E string }
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &set) != nil {
		return nil, errUnavailable
	}
	var key *rsa.PublicKey
	for _, j := range set.Keys {
		if j.Kid != kid || j.Kty != "RSA" || j.Alg != "" && j.Alg != "RS256" || j.Use != "" && j.Use != "sig" {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(j.N)
		eb, e2 := base64.RawURLEncoding.DecodeString(j.E)
		if e1 != nil || e2 != nil || len(eb) == 0 || len(eb) > 4 {
			return nil, errCorrupt
		}
		e := new(big.Int).SetBytes(eb).Int64()
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 || e < 3 || e%2 == 0 || key != nil {
			return nil, errCorrupt
		}
		key = &rsa.PublicKey{N: modulus, E: int(e)}
	}
	if key == nil {
		return nil, errCorrupt
	}
	return key, nil
}
