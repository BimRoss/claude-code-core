package threadownership

import (
	"path/filepath"
	"testing"

	"github.com/bimross/claude-code-core/threadowner"
)

// Compile-time: both backends satisfy the interface.
var _ Store = (*File)(nil)
var _ Store = enumStore{}

func TestFile_RoundTripAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".thread-owners.json")
	f := NewFile(path)

	if _, ok := f.Owner("C1", "T1"); ok {
		t.Fatal("empty store should report no owner")
	}
	if err := f.SetOwner("C1", "T1", "self"); err != nil {
		t.Fatal(err)
	}
	if id, ok := f.Owner("C1", "T1"); !ok || id != "self" {
		t.Fatalf("Owner = (%q,%v) want (self,true)", id, ok)
	}
	// Distinct thread, same channel — independent.
	if _, ok := f.Owner("C1", "T2"); ok {
		t.Fatal("T2 should be unowned")
	}
	if err := f.Clear("C1", "T1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Owner("C1", "T1"); ok {
		t.Fatal("Clear should remove ownership")
	}
	// Clear is idempotent.
	if err := f.Clear("C1", "T1"); err != nil {
		t.Fatalf("Clear should be idempotent: %v", err)
	}
}

// "ownsThread = id == self" — the semantic the owner-mode dispatcher relies on.
func TestFile_OwnsThreadSemantic(t *testing.T) {
	f := NewFile(filepath.Join(t.TempDir(), "o.json"))
	const self = "self"
	_ = f.SetOwner("C1", "T1", self)
	id, ok := f.Owner("C1", "T1")
	if !(ok && id == self) {
		t.Fatalf("owns should be true: id=%q ok=%v", id, ok)
	}
	id2, ok2 := f.Owner("C1", "T2")
	if ok2 && id2 == self {
		t.Fatal("unowned thread must not read as owned-by-self")
	}
}

func TestFile_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.json")
	f := NewFile(path)
	if err := f.SetOwner("C1", "T1", "ross"); err != nil {
		t.Fatal(err)
	}
	// Reopen from disk — the write must have persisted.
	f2 := NewFile(path)
	if id, ok := f2.Owner("C1", "T1"); !ok || id != "ross" {
		t.Fatalf("reopened store Owner = (%q,%v) want (ross,true)", id, ok)
	}
}

func TestFile_MissingFileStartsEmpty(t *testing.T) {
	f := NewFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if _, ok := f.Owner("C1", "T1"); ok {
		t.Fatal("missing file should start empty, not error")
	}
}

func TestFromEnum_RoundTripAndClearNoOp(t *testing.T) {
	// threadowner.New returns the file-backed enum store (no Redis URL).
	enum := threadowner.New(t.TempDir())
	s := FromEnum(enum)

	if err := s.SetOwner("C1", "T1", "joanne"); err != nil {
		t.Fatal(err)
	}
	if id, ok := s.Owner("C1", "T1"); !ok || id != "joanne" {
		t.Fatalf("Owner = (%q,%v) want (joanne,true)", id, ok)
	}
	// Flip to the other fleet member (default-mode routing does this).
	if err := s.SetOwner("C1", "T1", "ross"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.Owner("C1", "T1"); id != "ross" {
		t.Fatalf("flip failed, id=%q", id)
	}
	// Clear is a documented no-op for the enum adapter (default mode never
	// clears). Ownership must remain after Clear.
	if err := s.Clear("C1", "T1"); err != nil {
		t.Fatal(err)
	}
	if id, ok := s.Owner("C1", "T1"); !ok || id != "ross" {
		t.Fatalf("enum Clear must be a no-op; Owner = (%q,%v) want (ross,true)", id, ok)
	}
}
