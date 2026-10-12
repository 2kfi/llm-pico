package ratelimit

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/arkan/llm-pico/internal/store"
)

func newTestLimiter(t *testing.T) *Limiter {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "rl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestWindowStartBoundaries(t *testing.T) {
	// A fixed instant: 2026-03-05 14:37:42 UTC.
	now := time.Date(2026, 3, 5, 14, 37, 42, 0, time.UTC)

	cases := []struct {
		window Window
		want   string
	}{
		{RPM, "2026-03-05T14:37:00"},
		{TPM, "2026-03-05T14:37:00"},
		{ASH, "2026-03-05T14:00:00"},
		{RPD, "2026-03-05"},
		{TPD, "2026-03-05"},
		{ASD, "2026-03-05"},
	}
	for _, tc := range cases {
		if got := windowStart(tc.window, now); got != tc.want {
			t.Errorf("windowStart(%s) = %q, want %q", tc.window, got, tc.want)
		}
	}
}

func TestWindowEnd(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 37, 42, 0, time.UTC)

	if got, want := WindowEnd(RPM, now), time.Date(2026, 3, 5, 14, 38, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("WindowEnd(RPM) = %v, want %v", got, want)
	}
	if got, want := WindowEnd(ASH, now), time.Date(2026, 3, 5, 15, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("WindowEnd(ASH) = %v, want %v", got, want)
	}
	if got, want := WindowEnd(RPD, now), time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("WindowEnd(RPD) = %v, want %v", got, want)
	}
}

func TestCountWindowRejects(t *testing.T) {
	l := newTestLimiter(t)
	limits := Limits{RPM: Ptr(2)}

	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 100); r != nil {
		t.Fatalf("first request rejected: %v", r)
	}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 100); r != nil {
		t.Fatalf("second request rejected: %v", r)
	}

	r := l.CheckAndReserve("k", "m", LevelUser, limits, 100)
	if r == nil {
		t.Fatal("third request should exceed the rpm limit of 2")
	}
	if r.Window != RPM {
		t.Errorf("window = %s, want rpm", r.Window)
	}
	if r.Count != 2 {
		t.Errorf("count = %d, want 2", r.Count)
	}
	if r.RetryAfter <= 0 || r.RetryAfter > time.Minute {
		t.Errorf("retryAfter = %v, want (0, 1m]", r.RetryAfter)
	}
}

func TestTokenWindowUsesReservation(t *testing.T) {
	l := newTestLimiter(t)
	// TPM of 1000, each request reserving prompt+max_tokens.
	limits := Limits{TPM: Ptr(1000)}

	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 400); r != nil {
		t.Fatal("first request rejected")
	}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 400); r != nil {
		t.Fatal("second request rejected")
	}
	r := l.CheckAndReserve("k", "m", LevelUser, limits, 400)
	if r == nil {
		t.Fatal("third request should exceed tpm: 800+400 > 1000")
	}
	if r.Window != TPM {
		t.Errorf("window = %s, want tpm", r.Window)
	}
}

func TestRejectedRequestConsumesNothing(t *testing.T) {
	l := newTestLimiter(t)
	// RPM allows plenty, but TPM will refuse the second request.
	limits := Limits{RPM: Ptr(100), TPM: Ptr(1000)}

	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 900); r != nil {
		t.Fatal("first rejected unexpectedly")
	}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 900); r == nil {
		t.Fatal("second should have been rejected")
	}

	// RPM must still show exactly 1: the rejected request must not have
	// incremented it, because it failed on TPM.
	if got := l.Usage("k", "m", LevelUser, RPM); got != 1 {
		t.Errorf("rpm usage after rejection = %d, want 1", got)
	}
}

func TestReconcileAdjustsTokenCount(t *testing.T) {
	l := newTestLimiter(t)
	limits := Limits{TPM: Ptr(1000)}

	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 900); r != nil {
		t.Fatal("reserve rejected")
	}
	// Actual usage came in at 100, so 800 should be returned to the window.
	l.Reconcile("k", "m", LevelUser, 100, 900)

	if got := l.Usage("k", "m", LevelUser, TPM); got != 100 {
		t.Errorf("tpm usage = %d, want 100", got)
	}

	// The returned headroom is now usable.
	for i := 0; i < 3; i++ {
		if r := l.CheckAndReserve("k", "m", LevelUser, limits, 300); r != nil {
			t.Fatalf("request %d rejected after reconcile: %v", i+1, r)
		}
	}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 300); r == nil {
		t.Error("fourth request should exceed: 100 + 3*300 = 1000")
	}
}

func TestReconcileNeverGoesNegative(t *testing.T) {
	l := newTestLimiter(t)
	limits := Limits{TPM: Ptr(1000)}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 100); r != nil {
		t.Fatal("reserve rejected")
	}
	// Over-reporting usage must not push the counter below zero.
	l.Reconcile("k", "m", LevelUser, 0, 100)
	if got := l.Usage("k", "m", LevelUser, TPM); got != 0 {
		t.Errorf("tpm usage = %d, want 0", got)
	}
}

func TestNoLimitsConfigured(t *testing.T) {
	l := newTestLimiter(t)
	// The Python implementation raised NameError here by reading a
	// loop-scoped variable after the loop that defined it.
	for i := 0; i < 100; i++ {
		if r := l.CheckAndReserve("k", "m", LevelUser, Limits{}, 4096); r != nil {
			t.Fatalf("request %d rejected with no limits configured: %v", i, r)
		}
	}
	if got := l.Usage("k", "m", LevelUser, RPM); got != 0 {
		t.Errorf("usage = %d, want 0 with no limits", got)
	}
}

func TestLevelsAreIndependent(t *testing.T) {
	l := newTestLimiter(t)
	limits := Limits{RPM: Ptr(1)}

	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 1); r != nil {
		t.Fatal("user-level rejected")
	}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 1); r == nil {
		t.Fatal("second user-level request should be rejected")
	}
	// The model-level bucket is separate and still has capacity.
	if r := l.CheckAndReserve("k", "m", LevelModel, limits, 1); r != nil {
		t.Errorf("model-level rejected independently: %v", r)
	}
}

func TestDifferentModelsAreIndependent(t *testing.T) {
	l := newTestLimiter(t)
	limits := Limits{RPM: Ptr(1)}

	if r := l.CheckAndReserve("k", "model-a", LevelUser, limits, 1); r != nil {
		t.Fatal("model-a rejected")
	}
	if r := l.CheckAndReserve("k", "model-a", LevelUser, limits, 1); r == nil {
		t.Fatal("second model-a request should be rejected")
	}
	if r := l.CheckAndReserve("k", "model-b", LevelUser, limits, 1); r != nil {
		t.Errorf("model-b rejected independently: %v", r)
	}
}

func TestUsageResetsOnWindowRollover(t *testing.T) {
	l := newTestLimiter(t)
	base := time.Date(2026, 3, 5, 14, 37, 42, 0, time.UTC)
	clock := base
	l.now = func() time.Time { return clock }

	limits := Limits{RPM: Ptr(5)}
	for i := 0; i < 5; i++ {
		if r := l.CheckAndReserve("k", "m", LevelUser, limits, 1); r != nil {
			t.Fatalf("request %d rejected: %v", i, r)
		}
	}

	// Advance past the minute boundary.
	clock = base.Add(time.Minute)
	if got := l.Usage("k", "m", LevelUser, RPM); got != 0 {
		t.Errorf("usage after rollover = %d, want 0", got)
	}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 1); r != nil {
		t.Errorf("request rejected in new window: %v", r)
	}
}

func TestDailyCountersPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rl.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Burn most of a daily budget, then persist it.
	l1 := New(db)
	now := time.Date(2026, 3, 5, 9, 0, 0, 0, time.UTC)
	l1.now = func() time.Time { return now }
	limits := Limits{RPD: Ptr(10)}
	for i := 0; i < 7; i++ {
		if r := l1.CheckAndReserve("k", "m", LevelUser, limits, 1); r != nil {
			t.Fatalf("request %d rejected: %v", i, r)
		}
	}
	if err := l1.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// A fresh limiter over the same database must resume at 7, not 0.
	db2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	l2 := New(db2)
	l2.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if r := l2.CheckAndReserve("k", "m", LevelUser, limits, 1); r != nil {
			t.Fatalf("request %d rejected after restart: %v", i, r)
		}
	}
	r := l2.CheckAndReserve("k", "m", LevelUser, limits, 1)
	if r == nil {
		t.Fatal("11th request should exceed the daily limit of 10")
	}
}

func TestConcurrentReserveIsExact(t *testing.T) {
	l := newTestLimiter(t)
	const limit = 500

	limits := Limits{RPM: Ptr(limit)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if l.CheckAndReserve("k", "m", LevelUser, limits, 1) == nil {
					mu.Lock()
					accepted++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if accepted != limit {
		t.Errorf("accepted %d requests, want exactly %d", accepted, limit)
	}
	if got := l.Usage("k", "m", LevelUser, RPM); got != limit {
		t.Errorf("usage = %d, want %d", got, limit)
	}
}

func TestPurgeStaleDropsOldWindows(t *testing.T) {
	l := newTestLimiter(t)
	base := time.Date(2026, 3, 5, 14, 37, 0, 0, time.UTC)
	clock := base
	l.now = func() time.Time { return clock }

	limits := Limits{RPM: Ptr(100)}
	if r := l.CheckAndReserve("k", "m", LevelUser, limits, 1); r != nil {
		t.Fatal(r)
	}

	total := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		total += len(l.shards[i].m)
		l.shards[i].mu.Unlock()
	}
	if total == 0 {
		t.Fatal("expected an in-memory counter")
	}

	// Advance two hours; the RPM and ASH buckets are both stale.
	clock = base.Add(2 * time.Hour)
	l.purgeStale()

	for i := range l.shards {
		l.shards[i].mu.Lock()
		for k, c := range l.shards[i].m {
			if !persistent(k.window) {
				t.Errorf("stale non-persistent counter for %s survived purge", k.window)
			}
			_ = c
		}
		l.shards[i].mu.Unlock()
	}
}
