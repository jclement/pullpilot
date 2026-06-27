package state

import (
	"testing"
	"time"
)

func TestSoakFirstSeenStable(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	first := s.SeenAt("web", "sha256:aaa", t0)
	// A later observation of the same digest must return the original timestamp.
	again := s.SeenAt("web", "sha256:aaa", t0.Add(time.Hour))
	if !first.Equal(again) {
		t.Errorf("first-seen drifted: %v vs %v", first, again)
	}

	// Reload from disk: soak state must survive a restart.
	s2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted := s2.SeenAt("web", "sha256:aaa", t0.Add(2*time.Hour))
	if !persisted.Equal(first) {
		t.Errorf("first-seen not persisted: %v vs %v", persisted, first)
	}
}

func TestBadDigest(t *testing.T) {
	s, _ := Load(t.TempDir())
	if s.IsBad("sha256:bad") {
		t.Fatal("unexpected bad")
	}
	s.MarkBad("sha256:bad")
	if !s.IsBad("sha256:bad") {
		t.Error("should be bad")
	}
}

func TestMarkAppliedPrunesOldFirstSeen(t *testing.T) {
	s, _ := Load(t.TempDir())
	now := time.Now()
	s.SeenAt("web", "sha256:old", now)
	s.SeenAt("web", "sha256:new", now)
	s.MarkApplied("web", "sha256:new")
	if _, ok := s.FirstSeen["web/sha256:old"]; ok {
		t.Error("stale first-seen entry should be pruned after apply")
	}
	if _, ok := s.FirstSeen["web/sha256:new"]; !ok {
		t.Error("applied digest first-seen should be retained")
	}
}
