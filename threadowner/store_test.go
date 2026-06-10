package threadowner

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStore_GetMissingReturnsNoOwner(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)
	if owner, ok := s.Get("C1", "T1"); ok {
		t.Fatalf("expected no owner for fresh store, got %q", owner)
	}
}

func TestStore_SetThenGet(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)
	if err := s.Set("C1", "T1", OwnerJoanne); err != nil {
		t.Fatalf("Set: %v", err)
	}
	owner, ok := s.Get("C1", "T1")
	if !ok || owner != OwnerJoanne {
		t.Fatalf("expected (joanne, true), got (%q, %v)", owner, ok)
	}
}

func TestStore_OverwriteFlipsOwner(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)
	_ = s.Set("C1", "T1", OwnerJoanne)
	if err := s.Set("C1", "T1", OwnerRoss); err != nil {
		t.Fatalf("Set: %v", err)
	}
	owner, _ := s.Get("C1", "T1")
	if owner != OwnerRoss {
		t.Fatalf("expected ross after flip, got %q", owner)
	}
}

func TestStore_SetNoOpSkipsWrite(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)
	_ = s.Set("C1", "T1", OwnerRoss)
	statBefore, err := os.Stat(s.statePath())
	if err != nil {
		t.Fatalf("stat after first set: %v", err)
	}
	// Sleep-free no-op detection: rewrite the same owner; mtime should
	// be unchanged because save() is skipped when the recorded owner
	// already matches.
	if err := s.Set("C1", "T1", OwnerRoss); err != nil {
		t.Fatalf("Set (no-op): %v", err)
	}
	statAfter, err := os.Stat(s.statePath())
	if err != nil {
		t.Fatalf("stat after no-op: %v", err)
	}
	if !statAfter.ModTime().Equal(statBefore.ModTime()) {
		t.Fatalf("expected no-op Set to skip disk write, but mtime changed")
	}
}

func TestStore_DifferentThreadsDontCollide(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)
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

func TestStore_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1 := New(dir)
	_ = s1.Set("C1", "T1", OwnerJoanne)

	s2 := New(dir)
	owner, ok := s2.Get("C1", "T1")
	if !ok || owner != OwnerJoanne {
		t.Fatalf("second instance should see persisted owner, got (%q, %v)", owner, ok)
	}
}

func TestStore_AtomicTempFileLeftoverIsHarmless(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)
	// Simulate a crashed write: create a stray .tmp file before any Set.
	if err := os.MkdirAll(filepath.Join(dir, stateDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	if err := s.Set("C1", "T1", OwnerRoss); err != nil {
		t.Fatalf("Set after stray tmp: %v", err)
	}
	owner, ok := s.Get("C1", "T1")
	if !ok || owner != OwnerRoss {
		t.Fatalf("expected (ross, true), got (%q, %v)", owner, ok)
	}
}

func TestStore_ConcurrentSetsDoNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := newFileStore(dir)

	const writers = 8
	const writesEach = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < writesEach; j++ {
				owner := OwnerRoss
				if (id+j)%2 == 0 {
					owner = OwnerJoanne
				}
				_ = s.Set("C1", "T1", owner)
			}
		}(i)
	}
	wg.Wait()

	// The store must still be readable — concurrent writes must not
	// corrupt the JSON file. The final owner is non-deterministic but
	// must be one of the two valid values.
	owner, ok := s.Get("C1", "T1")
	if !ok {
		t.Fatal("expected an owner after concurrent writes")
	}
	if owner != OwnerRoss && owner != OwnerJoanne {
		t.Fatalf("expected ross or joanne, got %q", owner)
	}
}
