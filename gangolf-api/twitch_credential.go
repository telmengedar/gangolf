package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

type twitchCredential struct {
	revalidateAt time.Time
	diagnostic   string
	Login        string `json:"-"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

func (c twitchCredential) fingerprint() [32]byte {
	data, _ := json.Marshal([]string{c.Login, c.AccessToken, c.RefreshToken, c.ClientSecret})
	return sha256.Sum256(data)
}

func privateCredentialInfo(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0 && (directory && info.IsDir() || !directory && info.Mode().IsRegular() && stat.Nlink == 1)
}

func credentialRoot(directory string) (*os.Root, error) {
	info, err := os.Lstat(directory)
	if err != nil || !privateCredentialInfo(info, true) {
		return nil, errTwitchCredential
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errTwitchCredential
	}
	info, err = root.Stat(".")
	if err != nil || !privateCredentialInfo(info, true) {
		root.Close()
		return nil, errTwitchCredential
	}
	return root, nil
}

func readTwitchCredential(directory, ref string) (twitchCredential, error) {
	data, err := readProtectedCredential(directory, ref)
	var c twitchCredential
	if err != nil || strictJSON(data, &c) != nil || c.AccessToken == "" || c.RefreshToken == "" {
		return twitchCredential{}, errTwitchCredential
	}
	return c, nil
}

func readProfileCredential(directory string, p twitchProfile) (twitchCredential, error) {
	if p.Transport != "irc" {
		return readTwitchCredential(directory, p.CredentialRef)
	}
	data, err := readProtectedCredential(directory, p.CredentialRef)
	if err != nil {
		return twitchCredential{}, err
	}
	return parseLegacyCredential(data)
}

func parseLegacyCredential(data []byte) (twitchCredential, error) {
	var c twitchCredential
	if !utf8.Valid(data) {
		return c, errTwitchCredential
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.ContainsFunc(line, unicode.IsControl) {
			return c, errTwitchCredential
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || (key != "User" && key != "Token") || values[key] != "" || value == "" {
			return c, errTwitchCredential
		}
		values[key] = value
	}
	c.Login = strings.ToLower(values["User"])
	c.AccessToken = strings.TrimPrefix(values["Token"], "oauth:")
	if !validTwitchLogin(c.Login) || c.AccessToken == "" || strings.ContainsFunc(c.AccessToken, unicode.IsSpace) {
		return twitchCredential{}, errTwitchCredential
	}
	return c, nil
}

func readProtectedCredential(directory, ref string) ([]byte, error) {
	var c []byte
	if !safeSlug(ref) {
		return c, errTwitchCredential
	}
	root, err := credentialRoot(directory)
	if err != nil {
		return c, err
	}
	defer root.Close()
	expected, err := root.Lstat(ref)
	if err != nil || !privateCredentialInfo(expected, false) {
		return c, errTwitchCredential
	}
	file, err := root.OpenFile(ref, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return c, errTwitchCredential
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !privateCredentialInfo(info, false) || !os.SameFile(expected, info) {
		return c, errTwitchCredential
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, errTwitchCredential
	}
	return data, nil
}

func writeTwitchCredential(directory, ref string, c twitchCredential) error {
	if !safeSlug(ref) {
		return errTwitchCredential
	}
	root, err := credentialRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(ref)
	if err != nil || !privateCredentialInfo(info, false) {
		return errTwitchCredential
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errTwitchCredential
	}
	name := ".rotate-" + hex.EncodeToString(nonce)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errTwitchCredential
	}
	defer root.Remove(name)
	data, _ := json.Marshal(c)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errTwitchCredential
	}
	if root.Rename(name, ref) != nil {
		return errTwitchCredential
	}
	dir, err := root.Open(".")
	if err != nil {
		return errTwitchCredential
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errTwitchCredential
	}
	return nil
}
