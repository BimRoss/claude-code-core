// Command bot-handoff is the CLI shim around the scratchpad package so the
// Claude subprocess in Ross and Joanne pods can call it without writing Go.
//
// Channel ID is sourced from $SLACK_CHANNEL_ID and the Redis URL from
// $MAC_REDIS_URL. Both are required and the program exits 1 if either is
// missing — explicit failure is better than a silent global-scope write.
//
// Usage:
//
//	bot-handoff put <key>     (reads blob from stdin)
//	bot-handoff get <key>     (writes blob to stdout, exit 3 if missing)
//	bot-handoff del <key>
//
// Exit codes:
//
//	0  success
//	1  config or transport error (details on stderr)
//	2  usage error
//	3  get-missing-key (so callers can branch without parsing stderr)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bimross/claude-code-core/scratchpad"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var notFound notFoundErr
		if errors.As(err, &notFound) {
			os.Exit(3)
		}
		var usage usageErr
		if errors.As(err, &usage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

type notFoundErr struct{ key string }

func (e notFoundErr) Error() string { return "bot-handoff: key not found: " + e.key }

func run() error {
	if len(os.Args) < 3 {
		return usageErr{msg: "usage: bot-handoff <put|get|del> <key>"}
	}
	cmd, key := os.Args[1], os.Args[2]

	redisURL := os.Getenv("MAC_REDIS_URL")
	if redisURL == "" {
		return fmt.Errorf("bot-handoff: MAC_REDIS_URL is required")
	}
	channelID := os.Getenv("SLACK_CHANNEL_ID")
	if channelID == "" {
		return fmt.Errorf("bot-handoff: SLACK_CHANNEL_ID is required")
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("bot-handoff: parse MAC_REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	client, err := scratchpad.New(scratchpad.Config{RDB: rdb, ChannelID: channelID})
	if err != nil {
		return fmt.Errorf("bot-handoff: %w", err)
	}

	ctx := context.Background()
	switch cmd {
	case "put":
		blob, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("bot-handoff: read stdin: %w", err)
		}
		if err := client.Put(ctx, key, blob, scratchpad.PutOpts{}); err != nil {
			return fmt.Errorf("bot-handoff: %w", err)
		}
		return nil
	case "get":
		blob, err := client.Get(ctx, key)
		if errors.Is(err, scratchpad.ErrKeyNotFound) {
			return notFoundErr{key: key}
		}
		if err != nil {
			return fmt.Errorf("bot-handoff: %w", err)
		}
		_, err = os.Stdout.Write(blob)
		return err
	case "del":
		if err := client.Delete(ctx, key); err != nil {
			return fmt.Errorf("bot-handoff: %w", err)
		}
		return nil
	default:
		return usageErr{msg: "unknown subcommand: " + cmd + " (want put|get|del)"}
	}
}
