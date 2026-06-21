// Package scratchpad is a per-channel shared key/value buffer that lets
// Ross and Joanne hand structured artifacts to each other within a single
// Slack channel.
//
// Why this exists: each bot pod has its own PVC, so writing to local disk
// doesn't cross over. Slack file uploads aren't readable by the receiving
// bot via the MCP (different scope from the post path). The only persistent
// store both bots already share is Redis. Scratchpad formalizes that with
// a namespaced, size-capped, TTL'd helper so callers stop reinventing it.
//
// Strict per-channel isolation is the load-bearing security property. The
// channel ID is sourced once from the spawn env at construction time and
// folded into every Redis key as `bot_handoff:<channel_id>:<key>`. There
// is intentionally no API to address another channel's keys, so a Ross
// spawn in channel A cannot read or overwrite a handoff written from a
// Joanne spawn in channel B even if it knows the key name.
//
// Size cap is 5 MiB per blob. Redis itself accepts much larger values, but
// the makeacompany-ai Redis is sized for hot operational state and we don't
// want a single handoff to crowd it. Callers with larger artifacts should
// chunk or move to object storage; see claude-code-core#9 for the
// follow-up plan.
package scratchpad

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// MaxBlobBytes is the per-Put size cap. 5 MiB.
	MaxBlobBytes = 5 * 1024 * 1024

	// DefaultTTL is applied when PutOpts.TTL is zero.
	DefaultTTL = 1 * time.Hour

	// MaxTTL caps how long a handoff can live. Anything longer should go
	// to object storage, not Redis.
	MaxTTL = 24 * time.Hour

	keyPrefix = "bot_handoff"
)

// Errors a caller can sensibly branch on. Other failures (Redis transport,
// context cancellation) are returned as wrapped errors.
var (
	ErrKeyNotFound   = errors.New("scratchpad: key not found")
	ErrEmptyKey      = errors.New("scratchpad: key is empty")
	ErrBlobTooLarge  = fmt.Errorf("scratchpad: blob exceeds %d bytes", MaxBlobBytes)
	ErrTTLOutOfRange = fmt.Errorf("scratchpad: TTL must be > 0 and <= %s", MaxTTL)
	ErrNoChannel     = errors.New("scratchpad: channel ID is empty (set SLACK_CHANNEL_ID at construction)")
)

// Client is a channel-scoped scratchpad handle. Constructed once at startup
// and reused across goroutines; safe for concurrent use.
type Client struct {
	rdb       *redis.Client
	channelID string
}

// Config wires a Client. RDB and ChannelID are required. ChannelID is
// almost always sourced from `os.Getenv("SLACK_CHANNEL_ID")` by the caller.
// We deliberately do NOT read env in this package — callers may want to
// stub the channel in tests.
type Config struct {
	RDB       *redis.Client
	ChannelID string
}

// New constructs a Client. Returns ErrNoChannel when ChannelID is empty
// rather than silently defaulting, because a misconfigured channel ID
// would let writes from one channel leak into another's namespace.
func New(cfg Config) (*Client, error) {
	if cfg.RDB == nil {
		return nil, errors.New("scratchpad: RDB required")
	}
	cid := strings.TrimSpace(cfg.ChannelID)
	if cid == "" {
		return nil, ErrNoChannel
	}
	return &Client{rdb: cfg.RDB, channelID: cid}, nil
}

// PutOpts is optional. Zero-value gets DefaultTTL.
type PutOpts struct {
	// TTL overrides DefaultTTL. Must be in (0, MaxTTL].
	TTL time.Duration
}

// Put writes blob under key, scoped to this Client's channel. Overwrites
// any prior value at the same key. Errors on oversized blobs, empty keys,
// or out-of-range TTL; succeeds on first write and on overwrite alike.
func (c *Client) Put(ctx context.Context, key string, blob []byte, opts PutOpts) error {
	if c == nil {
		return errors.New("scratchpad: nil client")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrEmptyKey
	}
	if len(blob) > MaxBlobBytes {
		return ErrBlobTooLarge
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return ErrTTLOutOfRange
	}
	if err := c.rdb.Set(ctx, c.redisKey(key), blob, ttl).Err(); err != nil {
		return fmt.Errorf("scratchpad: set: %w", err)
	}
	return nil
}

// Get reads the blob at key, scoped to this Client's channel. Returns
// ErrKeyNotFound when the key is missing or expired; the caller can
// distinguish that from transport errors with errors.Is.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("scratchpad: nil client")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrEmptyKey
	}
	b, err := c.rdb.Get(ctx, c.redisKey(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scratchpad: get: %w", err)
	}
	return b, nil
}

// Delete removes a key from this channel's scratchpad. No-op if absent.
// Returns nil whether the key existed or not — callers don't usually care.
func (c *Client) Delete(ctx context.Context, key string) error {
	if c == nil {
		return errors.New("scratchpad: nil client")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrEmptyKey
	}
	if err := c.rdb.Del(ctx, c.redisKey(key)).Err(); err != nil {
		return fmt.Errorf("scratchpad: del: %w", err)
	}
	return nil
}

// ChannelID returns the channel this Client is bound to. Useful for log
// lines that want to disambiguate which scratchpad a hit came from.
func (c *Client) ChannelID() string {
	if c == nil {
		return ""
	}
	return c.channelID
}

func (c *Client) redisKey(userKey string) string {
	return keyPrefix + ":" + c.channelID + ":" + userKey
}
