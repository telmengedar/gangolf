package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func (s *server) bootstrapAdmin(ctx context.Context, name, path string) (string, string, error) {
	if !validAppName(&name, true, 256) || path == "" {
		return "", "", errUnavailable
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", "", errUnavailable
	}
	defer output.Close()
	if err = output.Chmod(0600); err != nil {
		return "", "", errUnavailable
	}
	info, err := output.Stat()
	if err != nil || !safeBootstrapOutput(info, info) {
		return "", "", errUnavailable
	}
	if err = lockResources(ctx); err != nil {
		return "", "", err
	}
	defer resourceLock.Unlock()
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		return "", "", err
	}
	for _, user := range cfg.Users {
		if user.Enabled && permissionsContain(user.Permissions, "admin") {
			return "", "", errCorrupt
		}
	}
	nodeID, err := s.graph.createAppProfile(ctx, cfg.serviceOwner, "", &name)
	if err != nil {
		return "", "", err
	}
	userID := strconv.FormatInt(nodeID, 10)
	if _, exists := cfg.Users[userID]; exists {
		return "", "", errCorrupt
	}
	idBytes, secretBytes := make([]byte, 16), make([]byte, 32)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", errUnavailable
	}
	if _, err = rand.Read(secretBytes); err != nil {
		return "", "", errUnavailable
	}
	keyID := base64.RawURLEncoding.EncodeToString(idBytes)
	if _, exists := cfg.APIKeys[keyID]; exists {
		return "", "", errUnavailable
	}
	token := keyID + "." + base64.RawURLEncoding.EncodeToString(secretBytes)
	if _, err = io.WriteString(output, token+"\n"); err != nil {
		return "", "", errUnavailable
	}
	if err = output.Sync(); err != nil {
		return "", "", errUnavailable
	}
	if err = output.Close(); err != nil {
		return "", "", errUnavailable
	}
	if err = verifyBootstrapOutput(path, info, token); err != nil {
		return "", "", err
	}
	hash := hmac.New(sha256.New, []byte(s.pepper))
	hash.Write([]byte(token))
	expiry := s.now().UTC().Add(30 * 24 * time.Hour)
	cfg.Users[userID] = grant{Enabled: true, Permissions: []string{"admin", "host"}, ProfileNodeID: userID, ManagedOnly: true}
	cfg.APIKeys[keyID] = keyGrant{UserID: userID, Hash: hash.Sum(nil), Enabled: true, Permissions: []string{"admin", "host"}, Scope: "api", ExpiresAt: &expiry}
	if err = s.graph.saveAuth(ctx, cfg); err != nil {
		return "", "", err
	}
	if err = verifyBootstrapOutput(path, info, token); err != nil {
		return "", "", errOutcomeUnknown
	}
	return userID, keyID, nil
}

func bootstrapAdminCommand(args []string) error {
	flags := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("output", "", "")
	name := flags.String("name", "Administrator", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return errUnavailable
	}
	s, _, err := bootstrap()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	userID, keyID, err := s.bootstrapAdmin(ctx, *name, *path)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"userId": userID, "keyId": keyID, "file": *path})
}

func safeBootstrapOutput(actual, original os.FileInfo) bool {
	stat, ok := actual.Sys().(*syscall.Stat_t)
	return ok && actual.Mode().IsRegular() && actual.Mode().Perm() == 0600 && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid()) && os.SameFile(actual, original)
}

func verifyBootstrapOutput(path string, original os.FileInfo, token string) error {
	info, err := os.Lstat(path)
	if err != nil || !safeBootstrapOutput(info, original) {
		return errUnavailable
	}
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errUnavailable
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !safeBootstrapOutput(info, original) {
		return errUnavailable
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(len(token)+2)))
	if err != nil || !bytes.Equal(content, []byte(token+"\n")) {
		return errUnavailable
	}
	if err = file.Sync(); err != nil {
		return errUnavailable
	}
	if err = file.Close(); err != nil {
		return errUnavailable
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errUnavailable
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil || closeErr != nil {
		return errUnavailable
	}
	info, err = os.Lstat(path)
	if err != nil || !safeBootstrapOutput(info, original) {
		return errUnavailable
	}
	return nil
}
