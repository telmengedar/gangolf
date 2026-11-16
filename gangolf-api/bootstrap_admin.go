package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (s *server) bootstrapAdmin(ctx context.Context, name, path string) (string, string, error) {
	result, err := s.operatorAdminKey(ctx, "", name, path, nil)
	return result.UserID, result.KeyID, err
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
