package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
)

type overlayPlayer struct {
	ListenAddr   string
	PlayerOrigin string
	ParentOrigin string
}

func canonicalOverlayOrigin(raw string) (string, bool) {
	if raw == "" || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r > 127 }) || strings.ContainsAny(raw, "?#\\") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || len(host) > 253 {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	} else {
		labels := strings.Split(strings.TrimSuffix(host, "."), ".")
		for _, label := range labels {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", false
				}
			}
		}
		last := labels[len(labels)-1]
		if _, err := strconv.ParseUint(last, 0, 64); err == nil {
			return "", false
		}
		if _, err := strconv.ParseUint(last, 10, 64); err == nil {
			return "", false
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return "", false
		}
		port = strconv.Itoa(n)
		if u.Scheme == "http" && n == 80 || u.Scheme == "https" && n == 443 {
			port = ""
		}
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, true
}

func overlayBindAddress(raw string) (string, bool) {
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", false
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || port == "" {
		return "", false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(n)), true
}

func configureOverlayPlayer(mainAddress string) (*overlayPlayer, error) {
	address := os.Getenv("GANGOLF_OVERLAY_PLAYER_LISTEN_ADDR")
	player := os.Getenv("GANGOLF_OVERLAY_PLAYER_ORIGIN")
	parent := os.Getenv("GANGOLF_OVERLAY_PARENT_ORIGIN")
	if address == "" && player == "" && parent == "" {
		return nil, nil
	}
	bind, ok := overlayBindAddress(address)
	main, valid := overlayBindAddress(mainAddress)
	p, pok := canonicalOverlayOrigin(player)
	a, aok := canonicalOverlayOrigin(parent)
	_, port, _ := net.SplitHostPort(bind)
	if !ok || !valid || !pok || !aok || p == a || bind == main && port != "0" {
		return nil, errUnavailable
	}
	return &overlayPlayer{ListenAddr: bind, PlayerOrigin: p, ParentOrigin: a}, nil
}

func (p *overlayPlayer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "strict-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name, kind := "", ""
	switch r.URL.Path {
	case "/overlays/player.html":
		name, kind = "player.html", "text/html; charset=utf-8"
	case "/overlays/player.js":
		name, kind = "player.js", "text/javascript; charset=utf-8"
	case "/overlays/overlay.css":
		name, kind = "overlay.css", "text/css; charset=utf-8"
	default:
		failure(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		failure(w, 405, "method_not_allowed")
		return
	}
	_, credentials := r.Header["Authorization"]
	if credentials || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.ContentLength != 0 {
		failure(w, 400, "invalid_request")
		return
	}
	policy := "default-src 'none'; script-src 'self' https://www.youtube.com https://s.ytimg.com; style-src 'self'; frame-src https://www.youtube.com; connect-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + p.ParentOrigin
	serveOverlayAsset(w, name, kind, policy, p)
}
