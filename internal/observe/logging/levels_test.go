package logging

import (
	"sync"
	"testing"
	"time"
)

func TestLevelsPrecedenceExpiryInspectRemove(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	levels, err := NewLevels(LevelInfo, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer levels.Close()
	if got := levels.Effective("tenant-a", "loop"); got != LevelInfo {
		t.Fatalf("default=%v", got)
	}
	if err := levels.SetGlobal(LevelWarn); err != nil {
		t.Fatal(err)
	}
	if err := levels.SetTenant("tenant-a", LevelDebug, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := levels.SetLoop("tenant-a", "loop", LevelError, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := levels.SetLoop("tenant-b", "loop", LevelDebug, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := levels.Effective("tenant-a", "loop"); got != LevelError {
		t.Fatalf("loop=%v", got)
	}
	if got := levels.Effective("tenant-a", "other"); got != LevelDebug {
		t.Fatalf("tenant=%v", got)
	}
	if got := levels.Effective("tenant-b", "loop"); got != LevelDebug {
		t.Fatalf("tenant+loop key collision: %v", got)
	}
	if got := levels.Inspect(); len(got.Overrides) != 3 {
		t.Fatalf("overrides=%d", len(got.Overrides))
	}
	now = now.Add(2 * time.Minute)
	levels.Expire()
	if got := levels.Effective("tenant-a", "loop"); got != LevelWarn {
		t.Fatalf("expired=%v", got)
	}
	levels.RemoveGlobal()
	if got := levels.Effective("tenant-a", "loop"); got != LevelInfo {
		t.Fatalf("removed global=%v", got)
	}
}

func TestLevelsRejectInvalidExpiryAndRaceMutations(t *testing.T) {
	now := time.Now()
	levels, _ := NewLevels(LevelInfo, time.Minute, time.Now)
	defer levels.Close()
	if err := levels.SetTenant("t", LevelDebug, time.Time{}); err == nil {
		t.Fatal("missing expiry accepted")
	}
	if err := levels.SetTenant("t", LevelDebug, now.Add(2*time.Minute)); err == nil {
		t.Fatal("overlong expiry accepted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = levels.Effective("t", "l")
				if i%2 == 0 {
					_ = levels.SetLoop("t", "l", LevelDebug, time.Now().Add(time.Second))
				} else {
					levels.RemoveLoop("t", "l")
				}
				levels.Expire()
			}
		}(i)
	}
	wg.Wait()
}
