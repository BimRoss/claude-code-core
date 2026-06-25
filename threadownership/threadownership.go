// Package threadownership is the unified thread-ownership store for the
// dispatcher (#24 S2). It collapses the two divergent stores the agents used —
// the fleet's shared Redis/file ENUM store (threadowner.Store, owner ∈
// {ross,joanne}) and the personal-agent's per-pod bool store — behind one
// id-valued interface with two backends:
//
//   - default mode (Ross/Joanne): FromEnum wraps the existing threadowner.Store
//     so the fleet keeps its shared keyspace; ids are "ross"/"joanne".
//   - personal/team (PA): NewFile is a per-pod id-valued JSON store; the only id
//     ever written is the agent's self id, so Owner()==self IS the "do we own
//     this thread?" bool ownergate needs.
//
// The dispatcher maps this store into gate.State per mode: default mode reads
// CurrentOwner = Owner() (as the fleet enum); owner mode reads OwnsThread =
// (Owner()==self).
package threadownership

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/bimross/claude-code-core/threadowner"
)

// Store is the id-valued thread-ownership surface the dispatcher depends on.
type Store interface {
	// Owner returns the id that owns (channel, threadTS) and whether one is set.
	Owner(channel, threadTS string) (id string, ok bool)
	// SetOwner records id as the owner of (channel, threadTS).
	SetOwner(channel, threadTS, id string) error
	// Clear removes any ownership of (channel, threadTS). Idempotent.
	Clear(channel, threadTS string) error
}

func key(channel, threadTS string) string { return channel + ":" + threadTS }

// --- File: per-pod id-valued store (personal/team) ---

// File is a per-pod, id-valued thread-ownership store persisted as JSON. One
// writer (one pod), so it caches in memory and persists on every mutation.
// Replaces the personal-agent's local bool store; PA writes only its self id,
// but the store holds arbitrary ids so it's reusable.
type File struct {
	path string
	mu   sync.Mutex
	m    map[string]string
}

type fileState struct {
	Owned map[string]string `json:"owned"`
}

// NewFile opens (or initializes) the store at path. A missing or corrupt file
// starts empty — ownership is a best-effort routing aid, never worth crashing a
// boot over.
func NewFile(path string) *File {
	f := &File{path: path, m: map[string]string{}}
	body, err := os.ReadFile(path)
	if err != nil {
		return f
	}
	var st fileState
	if json.Unmarshal(body, &st) == nil && st.Owned != nil {
		f.m = st.Owned
	}
	return f
}

func (f *File) Owner(channel, threadTS string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.m[key(channel, threadTS)]
	return id, ok
}

func (f *File) SetOwner(channel, threadTS, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key(channel, threadTS)] = id
	return f.persist()
}

func (f *File) Clear(channel, threadTS string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key(channel, threadTS))
	return f.persist()
}

// persist writes the map atomically (tmp + rename). Caller holds f.mu.
func (f *File) persist() error {
	if f.path == "" {
		return nil // disabled (tests)
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(fileState{Owned: f.m}, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// --- FromEnum: adapter over the fleet's enum store (default mode) ---

// FromEnum adapts the fleet's existing threadowner.Store (Redis/file,
// enum-valued) to the id-valued Store. ids are "ross"/"joanne".
//
// Clear is a NO-OP: threadowner.Store has no delete, and default-mode routing
// never clears ownership (it flips between the pair via SetOwner). Clear is an
// owner-mode operation (the File backend), so it is never called on this
// adapter in practice.
func FromEnum(s threadowner.Store) Store { return enumStore{s: s} }

type enumStore struct{ s threadowner.Store }

func (e enumStore) Owner(channel, threadTS string) (string, bool) {
	o, ok := e.s.Get(channel, threadTS)
	return string(o), ok
}

func (e enumStore) SetOwner(channel, threadTS, id string) error {
	return e.s.Set(channel, threadTS, threadowner.Owner(id))
}

func (e enumStore) Clear(channel, threadTS string) error { return nil }
