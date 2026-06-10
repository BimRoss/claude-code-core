package threadowner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	stateFile = "thread-owners.json"
	stateDir  = ".ross-loops"
)

type entry struct {
	Owner     Owner     `json:"owner"`
	UpdatedAt time.Time `json:"updated_at"`
}

type fileState struct {
	Threads map[string]*entry `json:"threads"`
}

// fileStore persists thread-owner records to disk. NOT shared across pods
// when each pod mounts its own RWO PVC — see claude-code-ross#322. Use
// redisStore in prod; fileStore is for local dev and as a fallback when
// MAC_REDIS_URL is unset.
type fileStore struct {
	workspaceBase string
	mu            sync.Mutex
	clock         func() time.Time
}

func newFileStore(workspaceBase string) *fileStore {
	return &fileStore{workspaceBase: workspaceBase, clock: time.Now}
}

func (s *fileStore) statePath() string {
	return filepath.Join(s.workspaceBase, stateDir, stateFile)
}

func (s *fileStore) load() (*fileState, error) {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &fileState{Threads: map[string]*entry{}}, nil
		}
		return nil, fmt.Errorf("read thread-owners.json: %w", err)
	}
	var st fileState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse thread-owners.json: %w", err)
	}
	if st.Threads == nil {
		st.Threads = map[string]*entry{}
	}
	return &st, nil
}

func (s *fileStore) save(st *fileState) error {
	dir := filepath.Join(s.workspaceBase, stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal thread-owners.json: %w", err)
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.statePath()); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func (s *fileStore) Get(channel, threadTS string) (Owner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return "", false
	}
	e, ok := st.Threads[Key(channel, threadTS)]
	if !ok || e == nil {
		return "", false
	}
	return e.Owner, true
}

func (s *fileStore) Set(channel, threadTS string, owner Owner) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if st.Threads == nil {
		st.Threads = map[string]*entry{}
	}
	k := Key(channel, threadTS)
	if e, ok := st.Threads[k]; ok && e != nil && e.Owner == owner {
		return nil
	}
	st.Threads[k] = &entry{Owner: owner, UpdatedAt: s.clock()}
	return s.save(st)
}
