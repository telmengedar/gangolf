package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func bootstrap() (*server, string, error) {
	base, err := url.Parse(os.Getenv("GANGOLF_DIVOID_API_URL"))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "localhost" || base.Hostname() == "127.0.0.1" || base.Hostname() == "::1"))) {
		return nil, "", errUnavailable
	}
	token := os.Getenv("GANGOLF_DIVOID_TOKEN")
	pepper := os.Getenv("GANGOLF_API_KEY_PEPPER")
	if token == "" || pepper == "" {
		return nil, "", errUnavailable
	}
	ids := make([]int64, 3)
	for i, name := range []string{"GANGOLF_DIVOID_ROOT_ID", "GANGOLF_DIVOID_MESSAGES_ID", "GANGOLF_DIVOID_CONFIG_ID"} {
		ids[i], err = strconv.ParseInt(os.Getenv(name), 10, 64)
		if err != nil || ids[i] <= 0 {
			return nil, "", errUnavailable
		}
	}
	if ids[0] == ids[1] || ids[0] == ids[2] || ids[1] == ids[2] {
		return nil, "", errUnavailable
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s := &server{graph: &graph{base: base, token: token, root: ids[0], group: ids[1], config: ids[2], http: client}, pepper: pepper, now: time.Now}
	s.media, err = configureMedia(s.graph)
	if err != nil {
		return nil, "", err
	}
	s.twitch = &twitchRuntime{client: productionTwitchClient(os.Getenv("GANGOLF_TWITCH_CREDENTIAL_DIR"))}
	addr := os.Getenv("GANGOLF_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	return s, addr, nil
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "admin-key" {
		return adminKeyCommand(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "bootstrap-admin" {
		return bootstrapAdminCommand(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "seed-media" {
		return seedMediaCommand(os.Args[2:])
	}
	handler, addr, err := bootstrap()
	if err != nil {
		return err
	}
	return serve(handler, addr)
}

func serve(handler *server, addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := handler.graph.loadConfig(check)
	cancel()
	if err != nil {
		return err
	}
	receiverDone := make(chan struct{})
	go func() { defer close(receiverDone); handler.runTwitch(ctx) }()
	defer func() { stop(); <-receiverDone }()
	srv := &http.Server{Addr: addr, Handler: handler, BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		handler.eventHub().shutdown()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func main() {
	if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print("gangolf-api stopped: startup or server failure")
		os.Exit(1)
	}
}
