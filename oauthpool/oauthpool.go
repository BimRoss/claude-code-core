// Package oauthpool is the shared OAuth-token pool for `claude` spawns across
// all three agents (Ross, Joanne, personal-agent).
//
// Tokens come from independent Claude Max accounts, each with its own ~5-hour
// rolling window. Spreading spawns across N accounts multiplies the effective
// limit and — more importantly — lets a spawn fail over to a healthy account
// when one is throttled.
//
// The pool is NOT fixed at two keys. It discovers every populated slot from the
// environment: CLAUDE_CODE_OAUTH_TOKEN is slot "1", and CLAUDE_CODE_OAUTH_TOKEN_2,
// _3, … _N are slots "2".."N". Add a key, add a slot — no code change. Round-robin
// runs only over populated slots, and failover iterates the whole pool rather than
// bouncing between exactly two.
//
// This package centralizes logic that had drifted across the three agents: the
// canonical version (from claude-code-joanne) skips empty slots and treats a
// single populated slot as "no alternate", which fixed both the 2026-06-20
// stray-API-key outage and the bash-child welcome-template bug. Extracting it
// here propagates that correctness to all three.
package oauthpool

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// baseEnvVar is slot 1; slots ≥2 are baseEnvVar + "_<n>".
const baseEnvVar = "CLAUDE_CODE_OAUTH_TOKEN"

// slotEnvPattern matches the pool env vars and captures the slot number for
// the _<n> form. The base var (no suffix) is slot 1.
var slotEnvPattern = regexp.MustCompile(`^CLAUDE_CODE_OAUTH_TOKEN(?:_([0-9]+))?$`)

// Slot is one resolved pool entry: a token and its stable label ("1", "2", …)
// used for logging and per-slot stats attribution.
type Slot struct {
	Label string
	Token string
}

// now is overridable in tests; the package never calls time.Now directly so
// cooldown logic is deterministic under test.
var now = time.Now

// Pool is the set of populated OAuth slots discovered from the environment,
// plus round-robin and cooldown state. Safe for concurrent use.
//
// Construct with FromEnv at startup (or per spawn — discovery is cheap and
// picks up any newly-provisioned slot). Round-robin order is preserved across
// the process via the shared counter so load spreads evenly over spawns.
type Pool struct {
	slots []Slot // populated, sorted by numeric slot

	mu       sync.Mutex
	cooldown map[string]time.Time // label -> time the slot becomes usable again
}

// counter round-robins across spawns for the whole process. Package-level so a
// fresh Pool per spawn still advances the rotation rather than resetting to 0.
var counter struct {
	sync.Mutex
	n uint64
}

// FromEnv discovers every populated pool slot from the current environment.
// Empty slots are skipped. The returned Pool is ready to use even when only
// one (or zero) slots are populated.
func FromEnv() *Pool {
	return fromEnviron(os.Environ())
}

func fromEnviron(environ []string) *Pool {
	byNum := map[int]string{}
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		key, val := kv[:i], kv[i+1:]
		m := slotEnvPattern.FindStringSubmatch(key)
		if m == nil || val == "" {
			continue
		}
		n := 1
		if m[1] != "" {
			// Parse error is impossible given the \d+ pattern, but guard anyway.
			if parsed, err := strconv.Atoi(m[1]); err == nil {
				n = parsed
			}
		}
		byNum[n] = val
	}

	nums := make([]int, 0, len(byNum))
	for n := range byNum {
		nums = append(nums, n)
	}
	sort.Ints(nums)

	slots := make([]Slot, 0, len(nums))
	for _, n := range nums {
		slots = append(slots, Slot{Label: strconv.Itoa(n), Token: byNum[n]})
	}
	return &Pool{slots: slots, cooldown: map[string]time.Time{}}
}

// Size is the number of populated slots.
func (p *Pool) Size() int { return len(p.slots) }

// Labels returns the slot labels in order ("1", "2", …). Useful for stats
// registries that pre-seed per-slot counters.
func (p *Pool) Labels() []string {
	out := make([]string, len(p.slots))
	for i, s := range p.slots {
		out[i] = s.Label
	}
	return out
}

// Next returns the slot to use for the next spawn, round-robining over
// populated slots and skipping any currently in cooldown (rate-limited until a
// known reset). If every slot is cooled down it returns the soonest-to-recover
// one rather than nothing — trying a likely-limited account beats not spawning.
//
// The second return is false only when the pool is empty (no token configured
// at all); callers treat that as "spawn without an explicit pool token".
func (p *Pool) Next() (Slot, bool) {
	if len(p.slots) == 0 {
		return Slot{}, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	t := now()

	// Advance the shared round-robin cursor once, then probe forward for the
	// first non-cooled slot starting at that offset.
	start := nextCounter() % uint64(len(p.slots))
	var fallback Slot
	var fallbackUntil time.Time
	for i := 0; i < len(p.slots); i++ {
		s := p.slots[(start+uint64(i))%uint64(len(p.slots))]
		until, cooling := p.cooldown[s.Label]
		if !cooling || !until.After(t) {
			return s, true
		}
		if fallback.Label == "" || until.Before(fallbackUntil) {
			fallback, fallbackUntil = s, until
		}
	}
	// All slots cooling — return the one recovering soonest.
	return fallback, true
}

// Others returns the populated slots NOT in `tried`, in round-robin order from
// the cursor, skipping cooled-down slots. This drives failover: after a
// transient fast-fail on one slot, the caller retries on the next healthy slot,
// iterating the whole pool until slots are exhausted rather than bouncing
// between two. Cooled-down slots are excluded (a known-limited account is not a
// useful retry target).
func (p *Pool) Others(tried map[string]bool) []Slot {
	if len(p.slots) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t := now()
	start := peekCounter() % uint64(len(p.slots))
	out := make([]Slot, 0, len(p.slots))
	for i := 0; i < len(p.slots); i++ {
		s := p.slots[(start+uint64(i))%uint64(len(p.slots))]
		if tried[s.Label] {
			continue
		}
		if until, cooling := p.cooldown[s.Label]; cooling && until.After(t) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// MarkLimited records that the slot with the given label is rate-limited until
// `until`, so Next/Others skip it. This is how a 429 (or a classified
// soft-throttle) on one account makes the pool fail over to the others until
// that account's window resets. A zero or past `until` clears any cooldown.
func (p *Pool) MarkLimited(label string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until.After(now()) {
		p.cooldown[label] = until
	} else {
		delete(p.cooldown, label)
	}
}

// HasAlternate reports whether, after failing on `triedLabel`, there is at
// least one other populated, non-cooled slot to retry on. Replaces the old
// two-key `hasSecondOAuthSlot`.
func (p *Pool) HasAlternate(triedLabel string) bool {
	return len(p.Others(map[string]bool{triedLabel: true})) > 0
}

func nextCounter() uint64 {
	counter.Lock()
	defer counter.Unlock()
	counter.n++
	return counter.n - 1
}

func peekCounter() uint64 {
	counter.Lock()
	defer counter.Unlock()
	return counter.n
}

// ScrubbedEnviron returns os.Environ() with provider API-key vars stripped, so
// a spawned `claude` always authenticates via the pool token the caller appends
// explicitly. `claude` prefers ANTHROPIC_API_KEY over CLAUDE_CODE_OAUTH_TOKEN,
// so a stray key in the pod env (e.g. a stale, credit-depleted one left behind
// for a one-off) silently breaks EVERY spawn — the 2026-06-20 Joanne outage.
// Filtering at the base makes spawns correct-by-construction regardless of pod
// env. Use this as the base of a spawn's cmd.Env, then append the pool token.
func ScrubbedEnviron() []string {
	return scrub(os.Environ())
}

func scrub(src []string) []string {
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
