package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

func TestTurnStateBucketsAreIsolatedByFullTuple(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	base := fp("ab", "c", "tools", 1)
	reg := s.Begin(base, false, false, now)
	reg.Store("saved", now)

	for _, other := range []codex.Fingerprint{
		fp("a", "bc", "tools", 1),
		fp("ab", "changed-system", "tools", 1),
		fp("ab", "c", "changed-tools", 1),
	} {
		if got := s.Begin(other, true, false, now).state; got != "" {
			t.Errorf("tuple %q/%q/%q read another bucket: %q", other.Key, other.System, other.Tools, got)
		}
	}
	modelOnly := base
	modelOnly.Model = "different-model"
	modelOnly.Reasoning = "different-reasoning"
	if got := s.Begin(modelOnly, true, false, now).state; got != "saved" {
		t.Errorf("model/control fields changed bucket identity: %q", got)
	}
}

func TestTurnThreadIDIsStableAndUnambiguous(t *testing.T) {
	first := fp("ab", "c", "", 1)
	if a, b := turnThreadID(turnKey(first)), turnThreadID(turnKey(first)); a != b {
		t.Fatalf("same tuple produced different IDs: %q and %q", a, b)
	}
	if a, b := turnThreadID(turnKey(first)), turnThreadID(turnKey(fp("a", "bc", "", 1))); a == b {
		t.Fatalf("ambiguous tuples produced the same ID: %q", a)
	}
	if got := len(turnThreadID(turnKey(first))); got != 64 {
		t.Errorf("SHA256 hex ID length = %d, want 64", got)
	}
}

func TestTurnStateTTLBoundary(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("saved", now)
	if got := s.Begin(f, true, false, now.Add(turnStateTTL-time.Nanosecond)).state; got != "saved" {
		t.Fatalf("state just before expiry = %q", got)
	}
	if got := s.Begin(f, true, false, now.Add(turnStateTTL)).state; got != "" {
		t.Errorf("state at expiry = %q, want empty", got)
	}
}

func TestTurnStateMissingStateRefreshDoesNotExtendTTL(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("saved", now)
	for i := 1; i <= 4; i++ {
		at := now.Add(time.Duration(i) * 5 * time.Minute)
		if got := s.Begin(f, true, false, at).state; got != "saved" {
			t.Fatalf("continuation %d state = %q", i, got)
		}
	}
	if got := s.Begin(f, true, false, now.Add(turnStateTTL)).state; got != "" {
		t.Errorf("repeated Begin extended old state to expiry: %q", got)
	}

	fresh := s.Begin(f, true, false, now.Add(turnStateTTL+time.Minute))
	fresh.Store("refreshed", now.Add(turnStateTTL+time.Minute))
	if got := s.Begin(f, true, false, now.Add(2*turnStateTTL)).state; got != "refreshed" {
		t.Errorf("fresh Store did not refresh TTL: %q", got)
	}
}

func TestTurnStateFIFOEvictsOldestWithoutReorderingRepeatedKeys(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	first := fp("key0000", "system", "tools", 1)
	s.Begin(first, false, false, now).Store("old", now)
	for i := 1; i < maxTurnStateBuckets; i++ {
		s.Begin(fp(fmt.Sprintf("key%04d", i), "system", "tools", 1), false, false, now)
	}
	s.Begin(first, true, false, now)
	s.Begin(fp("new-key", "system", "tools", 1), false, false, now)
	if len(s.buckets) != maxTurnStateBuckets {
		t.Fatalf("bucket count = %d, want %d", len(s.buckets), maxTurnStateBuckets)
	}
	if _, exists := s.buckets[turnKey(first)]; exists {
		t.Error("repeated first key was reordered instead of evicted")
	}
	if got := s.Begin(fp("key0001", "system", "tools", 1), true, false, now).threadID; got != turnThreadID(turnKey(fp("key0001", "system", "tools", 1))) {
		t.Errorf("newest key thread ID changed: %q", got)
	}
}

func TestTurnStateNewTurnClearsAndRejectsStaleCompletion(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("before", now)
	old := s.Begin(f, true, false, now.Add(time.Second))
	cleared := s.Begin(f, false, true, now.Add(2*time.Second))
	if !cleared.newTurn {
		t.Fatal("new turn was not reported")
	}
	old.Store("stale reply", now.Add(3*time.Second))
	if got := s.Begin(f, true, false, now.Add(4*time.Second)).state; got != "" {
		t.Fatalf("stale completion restored cleared state: %q", got)
	}
}

func TestTurnStateStaleCompletionCannotRestoreEvictedOrExpiredBucket(t *testing.T) {
	t.Run("eviction", func(t *testing.T) {
		s := &turnStateStore{}
		now := time.Unix(1000, 0)
		f := fp("evicted", "system", "tools", 1)
		stale := s.Begin(f, false, false, now)
		for i := 0; i < maxTurnStateBuckets; i++ {
			s.Begin(fp(fmt.Sprintf("other%04d", i), "system", "tools", 1), false, false, now)
		}
		s.Begin(f, false, false, now)
		stale.Store("late", now)
		if got := s.Begin(f, true, false, now).state; got != "" {
			t.Errorf("evicted registration restored state: %q", got)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		s := &turnStateStore{}
		now := time.Unix(1000, 0)
		f := fp("expired", "system", "tools", 1)
		stale := s.Begin(f, false, false, now)
		if got := s.Begin(f, true, false, now.Add(turnStateTTL)).state; got != "" {
			t.Fatalf("state at expiry = %q", got)
		}
		stale.Store("late", now.Add(turnStateTTL))
		if got := s.Begin(f, true, false, now.Add(turnStateTTL+time.Nanosecond)).state; got != "" {
			t.Errorf("expired registration restored state: %q", got)
		}
	})
}

func TestTurnStateWithoutKeyIsNotStored(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("", "system", "tools", 1)
	reg := s.Begin(f, false, false, now)
	if reg.threadID != turnThreadID(turnKey(f)) {
		t.Errorf("thread ID = %q", reg.threadID)
	}
	reg.Store("private state", now)
	if len(s.buckets) != 0 {
		t.Errorf("stored %d buckets without a key", len(s.buckets))
	}
	if got := s.Begin(f, true, false, now).state; got != "" {
		t.Errorf("replayed state without a key: %q", got)
	}
}

func TestTurnStateStoreConcurrentUse(t *testing.T) {
	s := &turnStateStore{}
	now := time.Unix(1000, 0)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				f := fp(fmt.Sprintf("key%d", (g*7+i)%80), fmt.Sprintf("system%d", i%3), "tools", 1)
				reg := s.Begin(f, i%2 == 0, i%17 == 0, now.Add(time.Duration(i)*time.Second))
				reg.Store(fmt.Sprintf("state-%d", i), now.Add(time.Duration(i)*time.Second))
			}
		}()
	}
	wg.Wait()
	if len(s.buckets) > maxTurnStateBuckets {
		t.Errorf("bucket count = %d, exceeds %d", len(s.buckets), maxTurnStateBuckets)
	}
}

func turnKey(f codex.Fingerprint) turnStateKey {
	return turnStateKey{key: f.Key, system: f.System, tools: f.Tools}
}
