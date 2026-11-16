package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"strconv"
	"time"
)

type operatorKey struct {
	UserID    string    `json:"userId"`
	KeyID     string    `json:"keyId"`
	ExpiresAt time.Time `json:"expiresAt"`
	File      string    `json:"file"`
}

func (s *server) operatorAdminKey(ctx context.Context, userID, name, path string, requestedExpiry *time.Time) (operatorKey, error) {
	if path == "" || userID == "" && !validAppName(&name, true, 256) {
		return operatorKey{}, errUnavailable
	}
	now := s.now().UTC()
	expiry := now.Add(30 * 24 * time.Hour)
	if requestedExpiry != nil {
		expiry = requestedExpiry.UTC()
	}
	if !expiry.After(now) || expiry.After(now.Add(365*24*time.Hour)) {
		return operatorKey{}, errUnavailable
	}
	if userID != "" {
		if _, ok := resourceID(userID); !ok {
			return operatorKey{}, errCorrupt
		}
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return operatorKey{}, errUnavailable
	}
	defer output.Close()
	if err = output.Chmod(0600); err != nil {
		return operatorKey{}, errUnavailable
	}
	info, err := output.Stat()
	if err != nil || !safeBootstrapOutput(info, info) {
		return operatorKey{}, errUnavailable
	}
	if err = lockResources(ctx); err != nil {
		return operatorKey{}, err
	}
	defer resourceLock.Unlock()
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return operatorKey{}, err
	}
	newUser := userID == ""
	if newUser {
		for _, user := range cfg.Users {
			if user.Enabled && permissionsContain(user.Permissions, "admin") {
				return operatorKey{}, errCorrupt
			}
		}
		nodeID, err := s.graph.createAppProfile(ctx, cfg.serviceOwner, "", &name)
		if err != nil {
			return operatorKey{}, err
		}
		userID = strconv.FormatInt(nodeID, 10)
		if _, exists := cfg.Users[userID]; exists {
			return operatorKey{}, errCorrupt
		}
	} else {
		user, exists := cfg.Users[userID]
		if !exists || !user.Enabled || !permissionsContain(user.Permissions, "admin") || user.ProfileNodeID == "" {
			return operatorKey{}, errCorrupt
		}
		if _, status := s.boundPrincipal(ctx, cfg, principal{UserID: userID}); status != 0 {
			return operatorKey{}, errCorrupt
		}
	}
	idBytes, secretBytes := make([]byte, 16), make([]byte, 32)
	if _, err = rand.Read(idBytes); err != nil {
		return operatorKey{}, errUnavailable
	}
	if _, err = rand.Read(secretBytes); err != nil {
		return operatorKey{}, errUnavailable
	}
	keyID := base64.RawURLEncoding.EncodeToString(idBytes)
	if _, exists := cfg.APIKeys[keyID]; exists {
		return operatorKey{}, errUnavailable
	}
	token := keyID + "." + base64.RawURLEncoding.EncodeToString(secretBytes)
	if _, err = io.WriteString(output, token+"\n"); err != nil {
		return operatorKey{}, errUnavailable
	}
	if err = output.Sync(); err != nil {
		return operatorKey{}, errUnavailable
	}
	if err = output.Close(); err != nil {
		return operatorKey{}, errUnavailable
	}
	if err = verifyBootstrapOutput(path, info, token); err != nil {
		return operatorKey{}, err
	}
	hash := hmac.New(sha256.New, []byte(s.pepper))
	hash.Write([]byte(token))
	if newUser {
		cfg.Users[userID] = grant{Enabled: true, Permissions: []string{"admin", "host"}, ProfileNodeID: userID, ManagedOnly: true}
	}
	cfg.APIKeys[keyID] = keyGrant{UserID: userID, Hash: hash.Sum(nil), Enabled: true, Permissions: []string{"admin", "host"}, Scope: "api", ExpiresAt: &expiry}
	if err = s.graph.saveAuth(ctx, cfg); err != nil {
		return operatorKey{}, err
	}
	if err = verifyBootstrapOutput(path, info, token); err != nil {
		return operatorKey{}, errOutcomeUnknown
	}
	return operatorKey{UserID: userID, KeyID: keyID, ExpiresAt: expiry, File: path}, nil
}
