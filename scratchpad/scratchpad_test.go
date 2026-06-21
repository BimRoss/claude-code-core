package scratchpad

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T, channelID string) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c, err := New(Config{RDB: rdb, ChannelID: channelID})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, mr
}

func TestNewRequiresChannel(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	if _, err := New(Config{RDB: rdb, ChannelID: ""}); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("want ErrNoChannel, got %v", err)
	}
	if _, err := New(Config{RDB: rdb, ChannelID: "   "}); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("want ErrNoChannel for whitespace, got %v", err)
	}
}

func TestNewRequiresRDB(t *testing.T) {
	if _, err := New(Config{RDB: nil, ChannelID: "C0X"}); err == nil {
		t.Fatal("want error for nil RDB, got nil")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	ctx := context.Background()
	want := []byte("hello world")
	if err := c.Put(ctx, "ttfv_dump", want, PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "ttfv_dump")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGetMissingReturnsErrKeyNotFound(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	_, err := c.Get(context.Background(), "absent")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound, got %v", err)
	}
}

// Cross-channel isolation is the load-bearing property of this package.
// A client bound to channel A must not see any key written by a client
// bound to channel B, even if both use the exact same user-key.
func TestCrossChannelIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	aClient, _ := New(Config{RDB: rdb, ChannelID: "C0AAA"})
	bClient, _ := New(Config{RDB: rdb, ChannelID: "C0BBB"})

	ctx := context.Background()
	if err := aClient.Put(ctx, "shared_key", []byte("from-A"), PutOpts{}); err != nil {
		t.Fatalf("A.Put: %v", err)
	}
	// B reads same user-key — must NOT see A's data.
	_, err := bClient.Get(ctx, "shared_key")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("cross-channel leak: B saw A's key (err=%v)", err)
	}
	// B writes its own value at the same user-key.
	if err := bClient.Put(ctx, "shared_key", []byte("from-B"), PutOpts{}); err != nil {
		t.Fatalf("B.Put: %v", err)
	}
	// A's value should still be intact.
	got, err := aClient.Get(ctx, "shared_key")
	if err != nil {
		t.Fatalf("A.Get: %v", err)
	}
	if string(got) != "from-A" {
		t.Fatalf("A's value was overwritten by B: got %q", got)
	}
}

func TestPutRejectsOversizedBlob(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	blob := make([]byte, MaxBlobBytes+1)
	err := c.Put(context.Background(), "big", blob, PutOpts{})
	if !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("want ErrBlobTooLarge, got %v", err)
	}
}

func TestPutAcceptsExactMaxSize(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	blob := make([]byte, MaxBlobBytes)
	if err := c.Put(context.Background(), "max", blob, PutOpts{}); err != nil {
		t.Fatalf("Put at max size: %v", err)
	}
}

func TestPutRejectsEmptyKey(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	err := c.Put(context.Background(), "   ", []byte("x"), PutOpts{})
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("want ErrEmptyKey, got %v", err)
	}
}

func TestPutRejectsOutOfRangeTTL(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	ctx := context.Background()
	if err := c.Put(ctx, "k", []byte("x"), PutOpts{TTL: -1}); !errors.Is(err, ErrTTLOutOfRange) {
		t.Fatalf("want ErrTTLOutOfRange for negative TTL, got %v", err)
	}
	if err := c.Put(ctx, "k", []byte("x"), PutOpts{TTL: MaxTTL + time.Second}); !errors.Is(err, ErrTTLOutOfRange) {
		t.Fatalf("want ErrTTLOutOfRange for over-cap TTL, got %v", err)
	}
}

func TestPutAppliesTTL(t *testing.T) {
	c, mr := newTestClient(t, "C0CHAN")
	ctx := context.Background()
	if err := c.Put(ctx, "ephemeral", []byte("x"), PutOpts{TTL: 30 * time.Second}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "ephemeral")
	if err != nil {
		t.Fatalf("Get before expiry: %v", err)
	}
	if string(got) != "x" {
		t.Fatalf("unexpected value: %q", got)
	}
	mr.FastForward(31 * time.Second)
	_, err = c.Get(ctx, "ephemeral")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound after TTL, got %v", err)
	}
}

func TestPutDefaultTTLIsOneHour(t *testing.T) {
	c, mr := newTestClient(t, "C0CHAN")
	ctx := context.Background()
	if err := c.Put(ctx, "k", []byte("x"), PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	mr.FastForward(59 * time.Minute)
	if _, err := c.Get(ctx, "k"); err != nil {
		t.Fatalf("Get at 59m: %v", err)
	}
	mr.FastForward(2 * time.Minute) // total 61m, past 1h default
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want expiry after 1h default, got %v", err)
	}
}

func TestPutOverwrites(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	ctx := context.Background()
	if err := c.Put(ctx, "k", []byte("v1"), PutOpts{}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := c.Put(ctx, "k", []byte("v2"), PutOpts{}); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v2" {
		t.Fatalf("got %q, want v2", got)
	}
}

func TestDelete(t *testing.T) {
	c, _ := newTestClient(t, "C0CHAN")
	ctx := context.Background()
	if err := c.Put(ctx, "k", []byte("x"), PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound after Delete, got %v", err)
	}
	// Idempotent: deleting an absent key returns nil.
	if err := c.Delete(ctx, "absent"); err != nil {
		t.Fatalf("Delete of absent key: %v", err)
	}
}

func TestRedisKeyShape(t *testing.T) {
	c, mr := newTestClient(t, "C0CHAN")
	if err := c.Put(context.Background(), "foo", []byte("x"), PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	keys := mr.Keys()
	want := "bot_handoff:C0CHAN:foo"
	found := false
	for _, k := range keys {
		if k == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected Redis key %q, got keys %v", want, keys)
	}
}

func TestNilClientReturnsError(t *testing.T) {
	var c *Client
	ctx := context.Background()
	if err := c.Put(ctx, "k", []byte("x"), PutOpts{}); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("want nil-receiver error from Put, got %v", err)
	}
	if _, err := c.Get(ctx, "k"); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("want nil-receiver error from Get, got %v", err)
	}
	if err := c.Delete(ctx, "k"); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("want nil-receiver error from Delete, got %v", err)
	}
	if got := c.ChannelID(); got != "" {
		t.Fatalf("nil ChannelID: got %q, want empty", got)
	}
}
