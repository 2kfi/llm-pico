// Package ratelimit enforces the rate-limit windows llm-pico supports
// against per-key and per-model budgets.
//
// A request reserves capacity before dispatch and reconciles against actual
// usage afterwards, so a long generation that consumes fewer tokens than
// reserved returns the difference, and a failed request refunds the whole
// reservation.
package ratelimit

import (
	"sync"
	"time"
)

// Window identifies one rate-limit dimension.
type Window string

const (
	RPM  Window = "rpm"  // requests per minute
	RPD  Window = "rpd"  // requests per day
	TPM  Window = "tpm"  // tokens per minute
	TPD  Window = "tpd"  // tokens per day
	CPM  Window = "cpm"  // characters per minute (TTS)
	CPD  Window = "cpd"  // characters per day (TTS)
	ASEC Window = "asec" // audio seconds per day (TTS/STT)
	AMIN Window = "amin" // audio minutes per day (STT)
)

// Windows lists every supported window in check order.
var Windows = []Window{RPM, TPM, RPD, TPD, CPM, CPD, ASEC, AMIN}

// Level distinguishes a per-key limit from a per-model limit. Both are
// enforced independently and a request must pass both.
type Level string

const (
	LevelUser  Level = "user"
	LevelModel Level = "model"
)

// Limits is a set of per-window ceilings. A nil window means unlimited.
type Limits map[Window]*int64

// HasAny reports whether at least one window is constrained.
func (l Limits) HasAny() bool {
	for _, v := range l {
		if v != nil {
			return true
		}
	}
	return false
}

func (l Limits) limit(w Window) (int64, bool) {
	v, ok := l[w]
	if !ok || v == nil {
		return 0, false
	}
	return *v, true
}

// Ptr is a convenience constructor for a window ceiling.
func Ptr(v int64) *int64 { return &v }

// Rejection describes which window refused a request and when to retry.
type Rejection struct {
	Window     Window
	Limit      int64
	Count      int64
	RetryAfter time.Duration
}

func (r *Rejection) Error() string {
	return "ratelimit: exceeded " + string(r.Window)
}

// Costs carries the per-domain estimates a reservation may charge. Only the
// windows a limit set constrains are read.
type Costs struct {
	Tokens   int64
	Chars    int64
	AudioSec int64
}

// AmountOf is how much one request consumes of w.
func AmountOf(w Window, c Costs) int64 {
	switch w {
	case RPM, RPD:
		return 1
	case TPM, TPD:
		return c.Tokens
	case CPM, CPD:
		return c.Chars
	case ASEC:
		return c.AudioSec
	case AMIN:
		return (c.AudioSec + 59) / 60
	default:
		return 1
	}
}

// counter is one window's state for one cell.
type counter struct {
	windowStart string
	count       int64
}

// cellKey identifies a counter. slot distinguishes the per-provider-slot cell
// from the per-model aggregate cell at the same level.
type cellKey struct {
	keyHash   string
	modelName string
	slot      string
	level     Level
	window    Window
}

// shard is an independent slice of the counter map. Sharding keeps unrelated
// keys from contending on a single mutex.
type shard struct {
	mu sync.Mutex
	m  map[cellKey]*counter
}

// Limiter enforces rate limits, holding counters in memory.
type Limiter struct {
	shards []shard
	mask   uint64

	purgeEvery time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// now is injectable so window boundaries can be tested deterministically.
	now func() time.Time
}

// shardCount must be a power of two.
const shardCount = 64

// New creates an in-memory limiter.
func New() *Limiter {
	l := &Limiter{
		shards:     make([]shard, shardCount),
		mask:       shardCount - 1,
		purgeEvery: 10 * time.Minute,
		stopCh:     make(chan struct{}),
		now:        time.Now,
	}
	for i := range l.shards {
		l.shards[i].m = make(map[cellKey]*counter)
	}
	return l
}

// Start launches the background purge loop.
func (l *Limiter) Start() {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		purge := time.NewTicker(l.purgeEvery)
		defer purge.Stop()
		for {
			select {
			case <-l.stopCh:
				return
			case <-purge.C:
				l.purgeStale()
			}
		}
	}()
}

// Stop halts the background loop.
func (l *Limiter) Stop() {
	l.stopOnce.Do(func() { close(l.stopCh) })
	l.wg.Wait()
}

// shardFor maps a request onto its counter shard.
//
// The window is deliberately excluded from the hash: all windows for a given
// cell must land in the same shard so CheckAndReserve can hold one lock across
// its validation and commit phases. Splitting them would let two concurrent
// requests both pass validation and then both commit, overshooting the limit.
func (l *Limiter) shardFor(k cellKey) *shard {
	h := fnv64(k.keyHash) ^ fnv64(k.modelName) ^ fnv64(k.slot) ^ uint64(len(k.level))<<32
	return &l.shards[h&l.mask]
}

func fnv64(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// windowStart returns the canonical bucket label for a window at time t.
func windowStart(w Window, t time.Time) string {
	utc := t.UTC()
	switch w {
	case RPM, TPM, CPM:
		return utc.Format("2006-01-02T15:04:00")
	default: // RPD, TPD, CPD, ASEC, AMIN
		return utc.Format("2006-01-02")
	}
}

// WindowEnd returns when the current bucket for w rolls over.
func WindowEnd(w Window, t time.Time) time.Time {
	utc := t.UTC()
	switch w {
	case RPM, TPM, CPM:
		return utc.Truncate(time.Minute).Add(time.Minute)
	default:
		return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	}
}

// counterFor returns the current bucket for a cell, creating it on rollover.
// The caller must hold the cell's shard lock.
func (l *Limiter) counterFor(sh *shard, k cellKey, w Window, now time.Time) *counter {
	start := windowStart(w, now)
	c := sh.m[k]
	if c == nil || c.windowStart != start {
		c = &counter{windowStart: start}
		sh.m[k] = c
	}
	return c
}

// stagedCharge is one committed window increment, kept so a later rejection
// in the same limit set can roll it back.
type stagedCharge struct {
	window Window
	amount int64
}

// CheckAndReserve tests every configured window and, only if all pass,
// commits the increments.
//
// Validation and commit run window by window under each cell's shard lock;
// a rejection rolls back the already-committed increments of earlier windows.
func (l *Limiter) CheckAndReserve(keyHash, modelName, slot string, level Level, limits Limits, c Costs) *Rejection {
	now := l.now()
	var pending []stagedCharge

	for _, w := range Windows {
		limit, ok := limits.limit(w)
		if !ok {
			continue
		}
		key := cellKey{keyHash: keyHash, modelName: modelName, slot: slot, level: level, window: w}
		sh := l.shardFor(key)

		sh.mu.Lock()
		counter := l.counterFor(sh, key, w, now)
		amount := AmountOf(w, c)
		if counter.count+amount > limit {
			sh.mu.Unlock()
			l.rollback(keyHash, modelName, slot, level, pending, now)
			return &Rejection{
				Window:     w,
				Limit:      limit,
				Count:      counter.count,
				RetryAfter: WindowEnd(w, now).Sub(now),
			}
		}
		counter.count += amount
		sh.mu.Unlock()
		pending = append(pending, stagedCharge{window: w, amount: amount})
	}
	return nil
}

func (l *Limiter) rollback(keyHash, modelName, slot string, level Level, pending []stagedCharge, now time.Time) {
	for _, s := range pending {
		key := cellKey{keyHash: keyHash, modelName: modelName, slot: slot, level: level, window: s.window}
		sh := l.shardFor(key)
		sh.mu.Lock()
		if c := sh.m[key]; c != nil && c.windowStart == windowStart(s.window, now) {
			c.count = max(0, c.count-s.amount)
		}
		sh.mu.Unlock()
	}
}

// Refund credits back a full reservation, e.g. when a request never reached
// an upstream. Windows outside the limit set are skipped.
func (l *Limiter) Refund(keyHash, modelName, slot string, level Level, limits Limits, c Costs) {
	now := l.now()
	for _, w := range Windows {
		if _, ok := limits.limit(w); !ok {
			continue
		}
		key := cellKey{keyHash: keyHash, modelName: modelName, slot: slot, level: level, window: w}
		sh := l.shardFor(key)
		sh.mu.Lock()
		if c2 := sh.m[key]; c2 != nil && c2.windowStart == windowStart(w, now) {
			c2.count = max(0, c2.count-AmountOf(w, c))
		}
		sh.mu.Unlock()
	}
}

// Reconcile returns the difference between actual and reserved usage. It must
// be called exactly once per successful reservation, otherwise token
// reservations leak.
func (l *Limiter) Reconcile(keyHash, modelName, slot string, level Level, actual, reserved int64) {
	delta := actual - reserved
	if delta == 0 {
		return
	}
	now := l.now()
	for _, w := range []Window{TPM, TPD} {
		key := cellKey{keyHash: keyHash, modelName: modelName, slot: slot, level: level, window: w}
		sh := l.shardFor(key)
		sh.mu.Lock()
		c := l.counterFor(sh, key, w, now)
		// A negative delta must not drive the counter below zero.
		c.count = max(0, c.count+delta)
		sh.mu.Unlock()
	}
}

// Usage reports the current count for one window, for the X-RateLimit headers.
func (l *Limiter) Usage(keyHash, modelName, slot string, level Level, w Window) int64 {
	key := cellKey{keyHash: keyHash, modelName: modelName, slot: slot, level: level, window: w}
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	c := sh.m[key]
	if c == nil || c.windowStart != windowStart(w, l.now()) {
		return 0
	}
	return c.count
}

// purgeStale drops counters whose bucket has rolled over, so memory does not
// grow without bound as keys come and go.
func (l *Limiter) purgeStale() {
	now := l.now()
	for i := range l.shards {
		l.shards[i].mu.Lock()
		for k, c := range l.shards[i].m {
			if c.windowStart != windowStart(k.window, now) {
				delete(l.shards[i].m, k)
			}
		}
		l.shards[i].mu.Unlock()
	}
}
