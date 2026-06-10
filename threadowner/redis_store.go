package threadowner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisKeyPrefix namespaces thread-owner keys in the shared
// makeacompany-ai-redis. Every key is `<prefix><channel>:<thread_ts>` or
// `<prefix><channel>:root` for the channel-root case.
const redisKeyPrefix = "makeacompany:thread_owner:"

// redisTTL ages out dead threads so the keyspace doesn't grow without
// bound. 30d matches the typical Slack thread relevance window — well
// past any active conversation, well short of indefinite retention.
const redisTTL = 30 * 24 * time.Hour

// redisOpTimeout is the per-operation budget. The thread-stickiness gate
// runs on every inbound message, so a backend hiccup must not stall the
// dispatcher — fail open with a short timeout rather than block.
const redisOpTimeout = 750 * time.Millisecond

// redisStore reads/writes thread-owner records to the shared
// makeacompany-ai-redis. Selected automatically by NewFromEnv when
// MAC_REDIS_URL is set. Closed Redis errors fall through to Get/Set
// returning no-owner / error so the dispatcher routes via fallback.
type redisStore struct {
	rdb *redis.Client
}

func newRedisStore(url string) (*redisStore, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse MAC_REDIS_URL: %w", err)
	}
	return &redisStore{rdb: redis.NewClient(opts)}, nil
}

func (s *redisStore) keyFor(channel, threadTS string) string {
	return redisKeyPrefix + Key(channel, threadTS)
}

func (s *redisStore) Get(channel, threadTS string) (Owner, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	v, err := s.rdb.Get(ctx, s.keyFor(channel, threadTS)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			// Real error (timeout, connection refused, etc.). Treat as
			// no-owner so the dispatcher falls back to default routing.
			// Logging is the caller's job — a verbose warn here would
			// spam on every message during a backend blip.
			return "", false
		}
		return "", false
	}
	owner := Owner(v)
	if owner != OwnerRoss && owner != OwnerJoanne {
		// Defensive: someone wrote a garbage value. Treat as no-owner.
		return "", false
	}
	return owner, true
}

func (s *redisStore) Set(channel, threadTS string, owner Owner) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	return s.rdb.Set(ctx, s.keyFor(channel, threadTS), string(owner), redisTTL).Err()
}
