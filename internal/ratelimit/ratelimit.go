// Package ratelimit enforces the six rate-limit windows llm-pico supports
// against per-key and per-model budgets.
//
// A request reserves capacity before dispatch and reconciles against actual
// usage afterwards, so a long generation that consumes fewer tokens than
// reserved returns the difference, and a failed request refunds the whole
// reservation.
package ratelimit

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"github.com/arkan/llm-pico/internal/store"
)

// Window identifies one rate-limit dimension.
type Window string

const (
	RPM Window = "rpm" // requests per minute
	RPD Window = "rpd" // requests per day
	TPM Window = "tpm" // tokens per minute
	TPD Window = "tpd" // tokens per day
	ASH Window = "ash" // requests per hour
	ASD Window = "asd" // requests per day (aggregate)
)

// Windows lists every supported window in check order.
var Windows = []Window{RPM, TPM, ASH, RPD, TPD, ASD}

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

// counter is one window's state for one (key, model, level) cell.
type counter struct {
	windowStart string
	count       int64
	dirty       bool
}

// cellKey identifies a counter.
type cellKey struct {
	keyHash   string
	modelName string
	level     Level
	window    Window
}

// shard is an independent slice of the counter map. Sharding keeps unrelated
// keys from contending on a single mutex.
type shard struct {
	mu sync.Mutex
	m  map[cellKey]*counter
}

// Limiter enforces rate limits, holding counters in memory and checkpointing
// the long-running windows (daily) to SQLite so limits survive a restart.
type Limiter struct {
	shards []shard
	mask   uint64

	db            *store.Store
	flushInterval time.Duration
	purgeEvery    time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// now is injectable so window boundaries can be tested deterministically.
	now func() time.Time
}

// shardCount must be a power of two.
const shardCount = 64

// New creates a limiter that checkpoints daily counters to db.
func New(db *store.Store) *Limiter {
	l := &Limiter{
		shards:        make([]shard, shardCount),
		mask:          shardCount - 1,
		db:            db,
		flushInterval: time.Minute,
		purgeEvery:    10 * time.Minute,
		stopCh:        make(chan struct{}),
		now:           time.Now,
	}
	for i := range l.shards {
		l.shards[i].m = make(map[cellKey]*counter)
	}
	return l
}

// Start launches the background flush and purge loops.
func (l *Limiter) Start() {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		flush := time.NewTicker(l.flushInterval)
		defer flush.Stop()
		purge := time.NewTicker(l.purgeEvery)
		defer purge.Stop()
		for {
			select {
			case <-l.stopCh:
				return
			case <-flush.C:
				_ = l.Flush(context.Background())
			case <-purge.C:
				l.purgeStale()
			}
		}
	}()
}

// Stop flushes remaining counters and halts the background loop.
func (l *Limiter) Stop(ctx context.Context) {
	l.stopOnce.Do(func() { close(l.stopCh) })
	l.wg.Wait()
	_ = l.Flush(ctx)
}

// shardFor maps a request onto its counter shard.
//
// The window is deliberately excluded from the hash: all six windows for a
// given key/model/level must land in the same shard so CheckAndReserve can
// hold one lock across both its validation and commit phases. Splitting them
// would let two concurrent requests both pass validation and then both commit,
// overshooting the limit.
func (l *Limiter) shardFor(keyHash, modelName string, level Level) *shard {
	h := fnv64(keyHash) ^ fnv64(modelName) ^ uint64(len(level))<<32
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

// windowStart returns the canonical bucket label for a window at time t. Every
// counter in a bucket shares one label, which doubles as the SQLite key, so
// comparisons are plain string comparisons.
func windowStart(w Window, t time.Time) string {
	utc := t.UTC()
	switch w {
	case RPM, TPM:
		return utc.Format("2006-01-02T15:04:00")
	case ASH:
		return utc.Format("2006-01-02T15:00:00")
	default: // RPD, TPD, ASD
		return utc.Format("2006-01-02")
	}
}

// WindowEnd returns when the current bucket for w rolls over.
func WindowEnd(w Window, t time.Time) time.Time {
	utc := t.UTC()
	switch w {
	case RPM, TPM:
		return utc.Truncate(time.Minute).Add(time.Minute)
	case ASH:
		return utc.Truncate(time.Hour).Add(time.Hour)
	default:
		return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	}
}

// reserveAmount is how much a single request consumes of a window.
// Count-based windows consume one request; token-based windows consume the
// reservation.
func reserveAmount(w Window, reservation int64) int64 {
	switch w {
	case RPM, RPD, ASH, ASD:
		return 1
	default: // TPM, TPD
		return reservation
	}
}

// persistent reports whether a window is checkpointed to SQLite. Minute and
// hour windows roll over faster than the flush interval, so persisting them
// is wasted I/O.
func persistent(w Window) bool {
	switch w {
	case RPD, TPD, ASD:
		return true
	default:
		return false
	}
}

// CheckAndReserve tests every configured window and, only if all pass,
// commits the increments.
//
// Validation and commit happen under a single shard lock. Releasing between
// the two phases would let concurrent requests for the same key each pass
// validation and then both commit, overshooting the limit.
func (l *Limiter) CheckAndReserve(keyHash, modelName string, level Level, limits Limits, reservation int64) *Rejection {
	now := l.now()
	sh := l.shardFor(keyHash, modelName, level)

	type staged struct {
		window Window
		amount int64
	}
	var pending []staged

	sh.mu.Lock()
	defer sh.mu.Unlock()

	for _, w := range Windows {
		limit, ok := limits.limit(w)
		if !ok {
			continue
		}
		start := windowStart(w, now)
		key := cellKey{keyHash: keyHash, modelName: modelName, level: level, window: w}

		c := sh.m[key]
		if c == nil || c.windowStart != start {
			// The bucket rolled over since this counter was last touched.
			sh.m[key] = l.restore(key, w, start)
			c = sh.m[key]
		}

		amount := reserveAmount(w, reservation)
		if c.count+amount > limit {
			return &Rejection{
				Window:     w,
				Limit:      limit,
				Count:      c.count,
				RetryAfter: WindowEnd(w, now).Sub(now),
			}
		}
		pending = append(pending, staged{window: w, amount: amount})
	}

	for _, s := range pending {
		key := cellKey{keyHash: keyHash, modelName: modelName, level: level, window: s.window}
		if c := sh.m[key]; c != nil {
			c.count += s.amount
			c.dirty = true
		}
	}
	return nil
}

// restore loads a counter for a bucket that is not in memory. Daily windows
// survive restarts in SQLite; shorter windows start empty because they have
// already rolled over.
func (l *Limiter) restore(key cellKey, w Window, start string) *counter {
	c := &counter{windowStart: start}
	if !persistent(w) || l.db == nil {
		return c
	}
	var n int64
	if err := l.db.Get(context.Background(), &n,
		`SELECT count FROM rate_counters
		 WHERE key_hash=? AND model_name=? AND level=? AND window_type=? AND window_start=?`,
		key.keyHash, key.modelName, string(key.level), string(w), start); err == nil {
		c.count = n
	}
	return c
}

// Reconcile returns the difference between actual and reserved usage. It must
// be called exactly once per successful CheckAndReserve, otherwise token
// reservations leak.
func (l *Limiter) Reconcile(keyHash, modelName string, level Level, actual, reserved int64) {
	delta := actual - reserved
	if delta == 0 {
		return
	}
	now := l.now()
	sh := l.shardFor(keyHash, modelName, level)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	for _, w := range []Window{TPM, TPD} {
		key := cellKey{keyHash: keyHash, modelName: modelName, level: level, window: w}
		start := windowStart(w, now)
		c := sh.m[key]
		if c == nil || c.windowStart != start {
			c = l.restore(key, w, start)
			sh.m[key] = c
		}
		// A negative delta must not drive the counter below zero.
		c.count = max(0, c.count+delta)
		c.dirty = true
	}
}

// Usage reports the current count for one window, for the X-RateLimit headers.
func (l *Limiter) Usage(keyHash, modelName string, level Level, w Window) int64 {
	sh := l.shardFor(keyHash, modelName, level)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	c := sh.m[cellKey{keyHash: keyHash, modelName: modelName, level: level, window: w}]
	if c == nil || c.windowStart != windowStart(w, l.now()) {
		return 0
	}
	return c.count
}

// Flush checkpoints dirty daily counters to SQLite.
func (l *Limiter) Flush(ctx context.Context) error {
	if l.db == nil {
		return nil
	}
	now := l.now()

	type pending struct {
		key   cellKey
		count int64
		start string
	}
	var rows []pending

	for i := range l.shards {
		l.shards[i].mu.Lock()
		for k, c := range l.shards[i].m {
			if !c.dirty || !persistent(k.window) || c.windowStart != windowStart(k.window, now) {
				continue
			}
			rows = append(rows, pending{key: k, count: c.count, start: c.windowStart})
		}
		l.shards[i].mu.Unlock()
	}
	if len(rows) == 0 {
		return nil
	}

	// Deterministic order keeps the write pattern reproducible under test.
	sort.Slice(rows, func(a, b int) bool {
		x, y := rows[a].key, rows[b].key
		if x.keyHash != y.keyHash {
			return x.keyHash < y.keyHash
		}
		if x.modelName != y.modelName {
			return x.modelName < y.modelName
		}
		return x.window < y.window
	})

	err := l.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO rate_counters (key_hash, model_name, level, window_type, window_start, count)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(key_hash, model_name, level, window_type, window_start)
				DO UPDATE SET count = excluded.count`,
				r.key.keyHash, r.key.modelName, string(r.key.level),
				string(r.key.window), r.start, r.count); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	for i := range l.shards {
		l.shards[i].mu.Lock()
		for _, r := range rows {
			if c := l.shards[i].m[r.key]; c != nil && c.count == r.count {
				c.dirty = false
			}
		}
		l.shards[i].mu.Unlock()
	}
	return nil
}

// purgeStale drops counters whose bucket has rolled over, so memory does not
// grow without bound as keys come and go.
//
// A stale counter for a non-persistent window is always dropped: those windows
// are never written to SQLite, so nothing is lost. A stale persistent counter
// is retained while dirty so its final value still reaches the database.
func (l *Limiter) purgeStale() {
	now := l.now()
	for i := range l.shards {
		l.shards[i].mu.Lock()
		for k, c := range l.shards[i].m {
			if c.windowStart == windowStart(k.window, now) {
				continue
			}
			if persistent(k.window) && c.dirty {
				continue
			}
			delete(l.shards[i].m, k)
		}
		l.shards[i].mu.Unlock()
	}
}
