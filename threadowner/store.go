// Package threadowner records which agent (Ross or Joanne) owns a Slack
// thread, so the two wrappers can route generic in-thread messages
// organically: once an agent has responded in a thread, subsequent
// non-@-mentioned messages route to that same agent until the user
// explicitly @-mentions the other.
//
// Two backing stores are supported via the Store interface:
//
//   - RedisStore (preferred): keys `makeacompany:thread_owner:<channel>:<ts>`
//     in the shared makeacompany-ai-redis. Both bots write the same keyspace
//     so cross-agent ownership is visible to both. Selected automatically
//     when MAC_REDIS_URL is set. See claude-code-ross#322 for the audit that
//     forced the move from file to Redis.
//   - FileStore: state in `<workspaceBase>/.ross-loops/thread-owners.json`.
//     Survives pod rollouts within a single bot, but each bot writes its
//     own file — works in local dev where there's only one binary, but
//     does NOT share state across Ross and Joanne in prod. Retained as the
//     fallback when MAC_REDIS_URL is unset.
//
// The Decide rule in routing.go is the same regardless of backing store.
package threadowner

// Owner identifies which agent owns a thread.
type Owner string

const (
	OwnerRoss   Owner = "ross"
	OwnerJoanne Owner = "joanne"
)

// Store is the read/write surface used by the dispatcher. Both Get and Set
// are best-effort: any I/O error is swallowed and surfaces as ok=false on
// Get and a returned error on Set (caller logs and continues). Concurrent
// callers are safe — each implementation owns its own synchronization.
type Store interface {
	Get(channel, threadTS string) (Owner, bool)
	Set(channel, threadTS string, owner Owner) error
}

// Key composes the canonical key for a (channel, thread_ts) pair, shared
// across both backends. Exported so callers can build keys for batch
// inspection if they ever need to.
func Key(channel, threadTS string) string {
	if threadTS == "" {
		return channel + ":root"
	}
	return channel + ":" + threadTS
}

// New returns a FileStore rooted at workspaceBase. Preserved for backward
// compatibility — call NewFromEnv to get the prod-correct behavior
// (Redis when MAC_REDIS_URL is set).
func New(workspaceBase string) Store {
	return newFileStore(workspaceBase)
}

// NewFromEnv returns the prod-correct Store: RedisStore when redisURL is
// non-empty, otherwise the FileStore rooted at workspaceBase. Callers
// should pass os.Getenv("MAC_REDIS_URL") as redisURL — the function does
// not read env itself so tests can drive it directly.
//
// On a non-empty redisURL that fails to parse or connect, NewFromEnv
// falls back to FileStore and returns the error so the caller can log
// (but operation continues, because thread-stickiness is non-critical).
func NewFromEnv(workspaceBase, redisURL string) (Store, error) {
	if redisURL == "" {
		return newFileStore(workspaceBase), nil
	}
	s, err := newRedisStore(redisURL)
	if err != nil {
		return newFileStore(workspaceBase), err
	}
	return s, nil
}
