package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"
)

func adminKeyCommand(args []string) error {
	flags := flag.NewFlagSet("admin-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	userID := flags.String("user-id", "", "")
	path := flags.String("output", "", "")
	rawExpiry := flags.String("expires-at", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" {
		return errUnavailable
	}
	if _, ok := resourceID(*userID); !ok {
		return errUnavailable
	}
	var expiry *time.Time
	explicitExpiry := false
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "expires-at" {
			explicitExpiry = true
		}
	})
	if explicitExpiry {
		parsed, err := time.Parse(time.RFC3339Nano, *rawExpiry)
		if err != nil {
			return errUnavailable
		}
		expiry = &parsed
	}
	s, _, err := bootstrap()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := s.operatorAdminKey(ctx, *userID, "", *path, expiry)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
