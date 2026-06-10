package threadowner

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisStore_GetMissingReturnsNoOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("newRedisStore: %v", err)
	}
	if owner, ok := s.Get("C1", "T1"); ok {
		t.Fatalf("expected no owner for fresh store, got %q", owner)
	}
}

func TestRedisStore_SetThenGet(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("newRedisStore: %v", err)
	}
	if err := s.Set("C1", "T1", OwnerJoanne); err != nil {
		t.Fatalf("Set: %v", err)
	}
	owner, ok := s.Get("C1", "T1")
	if !ok || owner != OwnerJoanne {
		t.Fatalf("expected (joanne, true), got (%q, %v)", owner, ok)
	}
}

func TestRedisStore_OverwriteFlipsOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("newRedisStore: %v", err)
	}
	_ = s.Set("C1", "T1", OwnerRoss)
	_ = s.Set("C1", "T1", OwnerJoanne)
	owner, _ := s.Get("C1", "T1")
	if owner != OwnerJoanne {
		t.Fatalf("expected overwrite to flip owner to joanne, got %q", owner)
	}
}

func TestRedisStore_DifferentThreadsDontCollide(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("newRedisStore: %v", err)
	}
	_ = s.Set("C1", "T1", OwnerJoanne)
	_ = s.Set("C1", "T2", OwnerRoss)
	_ = s.Set("C2", "T1", OwnerRoss)
	if owner, _ := s.Get("C1", "T1"); owner != OwnerJoanne {
		t.Errorf("C1:T1 owner: got %q want joanne", owner)
	}
	if owner, _ := s.Get("C1", "T2"); owner != OwnerRoss {
		t.Errorf("C1:T2 owner: got %q want ross", owner)
	}
	if owner, _ := s.Get("C2", "T1"); owner != OwnerRoss {
		t.Errorf("C2:T1 owner: got %q want ross", owner)
	}
}

func TestRedisStore_SharedAcrossInstancesSimulatesCrossPod(t *testing.T) {
	// Two distinct Store instances pointed at the same Redis simulate the
	// real scenario (#322): Ross writes, Joanne reads. With the file-backed
	// store this used to silently fail because each pod has its own PVC.
	mr := miniredis.RunT(t)
	ross, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("ross store: %v", err)
	}
	joanne, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("joanne store: %v", err)
	}
	// Joanne claims a thread.
	if err := joanne.Set("C1", "T1", OwnerJoanne); err != nil {
		t.Fatalf("Set on joanne: %v", err)
	}
	// Ross immediately sees the new owner.
	owner, ok := ross.Get("C1", "T1")
	if !ok || owner != OwnerJoanne {
		t.Fatalf("ross failed to see joanne's write: got (%q, %v)", owner, ok)
	}
	// Ross flips ownership back.
	if err := ross.Set("C1", "T1", OwnerRoss); err != nil {
		t.Fatalf("Set on ross: %v", err)
	}
	owner, ok = joanne.Get("C1", "T1")
	if !ok || owner != OwnerRoss {
		t.Fatalf("joanne failed to see ross's write: got (%q, %v)", owner, ok)
	}
}

func TestRedisStore_RootThreadKeyRoundtrips(t *testing.T) {
	// Empty thread_ts (channel-root message) maps to "<channel>:root" and
	// must round-trip cleanly. We do NOT separately test a literal "root"
	// thread_ts — Slack thread timestamps are always numeric ("<sec>.<us>")
	// so the collision can't happen in practice, but Key() inherits the
	// existing file-store schema where it would. Pre-existing limitation,
	// not introduced by the Redis backing.
	mr := miniredis.RunT(t)
	s, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("newRedisStore: %v", err)
	}
	_ = s.Set("C1", "", OwnerRoss)
	if owner, ok := s.Get("C1", ""); !ok || owner != OwnerRoss {
		t.Errorf("empty-ts roundtrip: got (%q, %v) want (ross, true)", owner, ok)
	}
}

func TestRedisStore_DefensiveOnGarbageValue(t *testing.T) {
	// Someone poked a non-owner value into the key directly. Get should
	// treat it as no-owner rather than returning garbage to the caller.
	mr := miniredis.RunT(t)
	s, err := newRedisStore("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("newRedisStore: %v", err)
	}
	mr.Set(redisKeyPrefix+"C1:T1", "santa")
	if owner, ok := s.Get("C1", "T1"); ok {
		t.Fatalf("expected no-owner on garbage value, got %q", owner)
	}
}

func TestNewFromEnv_EmptyURLReturnsFileStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFromEnv(dir, "")
	if err != nil {
		t.Fatalf("NewFromEnv empty url: %v", err)
	}
	if _, ok := s.(*fileStore); !ok {
		t.Fatalf("expected *fileStore, got %T", s)
	}
}

func TestNewFromEnv_RedisURLReturnsRedisStore(t *testing.T) {
	mr := miniredis.RunT(t)
	dir := t.TempDir()
	s, err := NewFromEnv(dir, "redis://"+mr.Addr())
	if err != nil {
		t.Fatalf("NewFromEnv redis: %v", err)
	}
	if _, ok := s.(*redisStore); !ok {
		t.Fatalf("expected *redisStore, got %T", s)
	}
}

func TestNewFromEnv_BadURLFallsBackToFileAndReturnsError(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFromEnv(dir, "not-a-valid-url")
	if err == nil {
		t.Fatalf("expected error on bad url")
	}
	if _, ok := s.(*fileStore); !ok {
		t.Fatalf("expected fallback to *fileStore, got %T", s)
	}
}
