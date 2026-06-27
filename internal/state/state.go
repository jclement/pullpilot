// Package state persists PullPilot's update bookkeeping in the data dir:
// when each new digest was first seen (for soak), the last digest applied per
// container, and digests known to fail health checks (never auto-retried).
package state

import (
	"encoding/json"
	"fmt"
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
	// Notified records "container/digest" pairs already announced, so a soaking
	// or monitor-only update is notified once per new digest, not every cycle.
	Notified map[string]bool `json:"notified"`
}

// Load reads state from dir/state.json, returning an empty state if absent.
func Load(dir string) (*State, error) {
	s := &State{
		path:        filepath.Join(dir, "state.json"),
		FirstSeen:   map[string]time.Time{},
		BadDigests:  map[string]bool{},
		LastApplied: map[string]string{},
		Notified:    map[string]bool{},
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		// Corrupt state would otherwise silently reset all soak/bad-digest
		// bookkeeping; surface it and back the bad file up rather than discard.
		_ = os.Rename(s.path, s.path+".corrupt")
		return nil, fmt.Errorf("state.json is corrupt (backed up to %s.corrupt): %w", s.path, err)
	}
	if s.FirstSeen == nil {
		s.FirstSeen = map[string]time.Time{}
	}
	if s.BadDigests == nil {
		s.BadDigests = map[string]bool{}
	}
	if s.LastApplied == nil {
		s.LastApplied = map[string]string{}
	}
	if s.Notified == nil {
		s.Notified = map[string]bool{}
	}
	return s, nil
}

// FirstNotify reports whether this is the first notification for a given
// container/digest, recording it so repeated cycles don't re-notify.
func (s *State) FirstNotify(key, digest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key + "/" + digest
	if s.Notified[k] {
		return false
	}
	s.Notified[k] = true
	_ = s.save()
	return true
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

// PeekSeen returns the recorded first-seen time for a container/digest without
// recording one (read-only views). ok is false if it has never been seen.
func (s *State) PeekSeen(key, digest string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.FirstSeen[key+"/"+digest]
	return t, ok
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
