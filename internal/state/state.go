// Package state persists PullPilot's update bookkeeping in the data dir:
// when each new digest was first seen (for soak), the last digest applied per
// container, and digests known to fail health checks (never auto-retried).
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the on-disk bookkeeping, guarded by a mutex and flushed atomically.
type State struct {
	mu   sync.Mutex
	path string

	// FirstSeen maps "container/digest" -> first time we observed that digest
	// as the upstream target for the container. Drives the soak window.
	FirstSeen map[string]time.Time `json:"first_seen"`
	// BadDigests records digests that failed a health check; never retried.
	BadDigests map[string]bool `json:"bad_digests"`
	// LastApplied maps container key -> digest last successfully applied.
	LastApplied map[string]string `json:"last_applied"`
}

// Load reads state from dir/state.json, returning an empty state if absent.
func Load(dir string) (*State, error) {
	s := &State{
		path:        filepath.Join(dir, "state.json"),
		FirstSeen:   map[string]time.Time{},
		BadDigests:  map[string]bool{},
		LastApplied: map[string]string{},
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	_ = json.Unmarshal(data, s)
	if s.FirstSeen == nil {
		s.FirstSeen = map[string]time.Time{}
	}
	if s.BadDigests == nil {
		s.BadDigests = map[string]bool{}
	}
	if s.LastApplied == nil {
		s.LastApplied = map[string]string{}
	}
	return s, nil
}

func (s *State) save() error {
	tmp := s.path + ".tmp"
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SeenAt records (once) the first time a container's target digest was observed
// and returns that timestamp. now lets callers inject the clock in tests.
func (s *State) SeenAt(key, digest string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key + "/" + digest
	if t, ok := s.FirstSeen[k]; ok {
		return t
	}
	s.FirstSeen[k] = now
	_ = s.save()
	return now
}

// MarkBad flags a digest as health-check-failing so it is never retried.
func (s *State) MarkBad(digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BadDigests[digest] = true
	_ = s.save()
}

// IsBad reports whether a digest previously failed a health check.
func (s *State) IsBad(digest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BadDigests[digest]
}

// MarkApplied records the digest last applied for a container and prunes that
// container's stale first-seen entries.
func (s *State) MarkApplied(key, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastApplied[key] = digest
	for k := range s.FirstSeen {
		if len(k) > len(key) && k[:len(key)+1] == key+"/" && k != key+"/"+digest {
			delete(s.FirstSeen, k)
		}
	}
	_ = s.save()
}
