// Package router selects which provider, and which credential within that
// provider, serves the next request.
//
// Selection is health-weighted across every provider group configured for a
// model, with round-robin rotation of keys inside each group and a circuit
// breaker that removes unhealthy groups from rotation.
//
// A Router is immutable after construction: routing state is swapped wholesale
// on config reload rather than mutated in place, so readers never block and an
// in-flight request keeps a consistent view.
package router

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/arkan/llm-pico/internal/config"
)

// BreakerState is the circuit breaker lifecycle position.
type BreakerState int

const (
	StateClosed BreakerState = iota
	StateOpen
	StateHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "CLOSED"
	}
}

// ErrAllKeysCoolingDown reports that every credential for a model is inside a
// rate-limit cooldown. Callers surface this as 429 with a Retry-After.
var ErrAllKeysCoolingDown = errors.New("router: all keys are rate-limited")

// CooldownError carries the retry delay alongside ErrAllKeysCoolingDown.
type CooldownError struct {
	RetryAfter time.Duration
}

func (e *CooldownError) Error() string { return ErrAllKeysCoolingDown.Error() }
func (e *CooldownError) Unwrap() error { return ErrAllKeysCoolingDown }

// keyState tracks the health of one credential within a provider group.
type keyState struct {
	apiKey string

	mu            sync.Mutex
	cooldownUntil time.Time
	fails         int
}

// breaker tracks the health of a provider group as a whole.
type breaker struct {
	mu               sync.Mutex
	state            BreakerState
	errorCount       int
	openedAt         time.Time
	failureThreshold int
	recoveryTimeout  time.Duration
}

func (b *breaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errorCount++
	if b.errorCount >= b.failureThreshold {
		b.state = StateOpen
		b.openedAt = time.Now()
	}
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.errorCount = 0
	b.openedAt = time.Time{}
}

// allows reports whether a request may be sent. A breaker past its recovery
// timeout transitions to half-open and admits a single probe.
func (b *breaker) allows() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.openedAt) >= b.recoveryTimeout {
			b.state = StateHalfOpen
			return true
		}
		return false
	default:
		return true
	}
}

func (b *breaker) snapshot() (BreakerState, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state, b.errorCount
}

// latencyWindow is a bounded rolling window of observed latencies.
type latencyWindow struct {
	mu      sync.Mutex
	samples []time.Duration
	max     int
}

func (w *latencyWindow) record(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, d)
	if len(w.samples) > w.max {
		w.samples = w.samples[len(w.samples)-w.max:]
	}
}

func (w *latencyWindow) percentiles() (p50, p95 time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.samples) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), w.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Median: middle element for odd counts, mean of the two middle elements
	// for even counts.
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		p50 = sorted[mid]
	} else {
		p50 = (sorted[mid-1] + sorted[mid]) / 2
	}

	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return p50, sorted[idx]
}

// group is one provider (identified by slug + api_base) serving a model,
// along with every credential configured against it.
type group struct {
	slug    string
	apiBase string
	keys    []*keyState
	breaker breaker
	latency latencyWindow

	mu       sync.Mutex
	nextKey  int
	requests int
	errors   int
	health   float64
}

// healthScore is the availability-derived weight used to pick between groups.
// 1.0 means every request has succeeded.
func (g *group) healthScore() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.health
}

// pickKey advances the round-robin cursor and returns the next eligible key,
// skipping any currently cooling down. ok is false when all are cooling.
func (g *group) pickKey(now time.Time) (*keyState, time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	eligible := make([]*keyState, 0, len(g.keys))
	var earliest time.Time
	for _, k := range g.keys {
		k.mu.Lock()
		cd := k.cooldownUntil
		k.mu.Unlock()
		if cd.After(now) {
			if earliest.IsZero() || cd.Before(earliest) {
				earliest = cd
			}
			continue
		}
		eligible = append(eligible, k)
	}
	if len(eligible) == 0 {
		return nil, earliest, false
	}

	k := eligible[g.nextKey%len(eligible)]
	g.nextKey = (g.nextKey + 1) % len(eligible)
	return k, earliest, true
}

func (g *group) recordSuccess() {
	g.mu.Lock()
	g.requests++
	g.recomputeHealthLocked()
	g.mu.Unlock()
	g.breaker.recordSuccess()

	// A success on any credential clears the failure streak on all of them:
	// a 429 on one key is usually provider-wide, not credential-specific.
	for _, k := range g.keys {
		k.mu.Lock()
		k.fails = 0
		k.mu.Unlock()
	}
}

func (g *group) recordFailure(key *keyState, statusCode int) {
	g.mu.Lock()
	g.requests++
	g.errors++
	g.recomputeHealthLocked()
	g.mu.Unlock()

	switch {
	case statusCode == 429:
		key.mu.Lock()
		// Progressive backoff: stay short for the first few, then back off hard.
		cooldown := 10 * time.Second
		if key.fails >= 3 {
			cooldown = 30 * time.Second
		}
		key.cooldownUntil = time.Now().Add(cooldown)
		key.fails++
		key.mu.Unlock()

	case statusCode == 401 || statusCode == 402 || statusCode == 403:
		// Invalid key, exhausted balance, or revoked: quarantine for a year.
		key.mu.Lock()
		key.cooldownUntil = time.Now().Add(365 * 24 * time.Hour)
		key.mu.Unlock()

	case statusCode >= 500 && statusCode < 600:
		g.breaker.recordFailure()
	}
}

func (g *group) recomputeHealthLocked() {
	total := max(1, g.requests)
	errRate := float64(g.errors) / float64(total)
	availability := 1 - errRate
	// Dampen small error counts so one failure on a low-traffic group does not
	// zero it out entirely.
	errorFactor := 1 / max(0.01, errRate+0.01)
	g.health = min(1.0, availability*min(1.0, errorFactor))
}

// Router resolves a model name to a provider group, credential and model entry.
type Router struct {
	modelNames []string
	entries    map[string]config.ModelEntry
	groups     map[string][]*group

	// Per-model consecutive-failure tracking with a cooldown, so a model whose
	// upstream is gone stops absorbing traffic.
	mu             sync.Mutex
	modelFailures  map[string]int
	modelCooldown  map[string]time.Time
	cooldownWindow time.Duration
}

// New builds a routing table from a configuration snapshot.
func New(cfg *config.Config) *Router {
	r := &Router{
		entries:        make(map[string]config.ModelEntry, len(cfg.ModelList)),
		groups:         make(map[string][]*group, len(cfg.ModelList)),
		modelFailures:  map[string]int{},
		modelCooldown:  map[string]time.Time{},
		cooldownWindow: time.Duration(cfg.RouterSettings.CooldownTime) * time.Second,
	}
	if r.cooldownWindow <= 0 {
		r.cooldownWindow = 60 * time.Second
	}

	for _, entry := range cfg.ModelList {
		r.entries[entry.ModelName] = entry

		slug := entry.ProviderSlug()
		var target *group
		for _, g := range r.groups[entry.ModelName] {
			if g.slug == slug && g.apiBase == entry.ModelParams.APIBase {
				target = g
				break
			}
		}
		if target == nil {
			target = &group{
				slug:    slug,
				apiBase: entry.ModelParams.APIBase,
				health:  1.0,
				latency: latencyWindow{max: 100},
				breaker: breaker{
					failureThreshold: cfg.RouterSettings.CircuitBreaker.FailureThreshold,
					recoveryTimeout:  time.Duration(cfg.RouterSettings.CircuitBreaker.RecoveryTimeout) * time.Second,
				},
			}
			r.groups[entry.ModelName] = append(r.groups[entry.ModelName], target)
		}

		for _, k := range entry.Keys() {
			target.keys = append(target.keys, &keyState{apiKey: k})
		}
		r.modelNames = append(r.modelNames, entry.ModelName)
	}
	return r
}

// ModelNames returns every configured model name.
func (r *Router) ModelNames() []string { return r.modelNames }

// Target is the outcome of a successful resolution.
type Target struct {
	ProviderSlug string
	APIBase      string
	APIKey       string
	Model        config.ModelEntry
}

// health marks the resolved provider as healthy after a successful call.
type groupRef = *group

// Resolve selects a provider group and credential for modelName.
//
// Selection is weighted by group health score, so a flaky provider receives
// proportionally less traffic. Returns ErrAllKeysCoolingDown when every
// credential is in cooldown.
func (r *Router) Resolve(modelName string) (*Target, error) {
	entry, ok := r.entries[modelName]
	if !ok {
		return nil, ErrModelNotFound
	}

	if r.isCoolingDown(modelName) {
		return nil, ErrModelNotFound
	}

	now := time.Now()
	candidates := r.groups[modelName]

	var (
		pickedGroup *group
		pickedKey   *keyState
		earliest    time.Time
	)
	for _, g := range candidates {
		if !g.breaker.allows() {
			continue
		}
		k, next, ok := g.pickKey(now)
		if !ok {
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
			continue
		}
		if pickedGroup == nil || r.weightedBetter(g.healthScore(), pickedGroup.healthScore()) {
			pickedGroup, pickedKey = g, k
		}
	}

	if pickedGroup == nil {
		if !earliest.IsZero() {
			return nil, &CooldownError{RetryAfter: max(time.Second, earliest.Sub(now))}
		}
		return nil, ErrModelNotFound
	}

	return &Target{
		ProviderSlug: pickedGroup.slug,
		APIBase:      pickedGroup.apiBase,
		APIKey:       pickedKey.apiKey,
		Model:        entry,
	}, nil
}

// ErrModelNotFound reports that no provider currently serves the model.
var ErrModelNotFound = errors.New("router: model not found or cooling down")

// weightedBetter decides whether candidate should displace incumbent in the
// weighted draw. Using a single random comparison implements weighted selection
// without needing floating-point cumulative weights.
func (r *Router) weightedBetter(candidate, incumbent float64) bool {
	cw := max(0.01, candidate)
	iw := max(0.01, incumbent)
	return rand.Float64() < cw/(cw+iw)
}

// RecordSuccess credits a successful request to the resolved target.
func (r *Router) RecordSuccess(t *Target) {
	if g := r.find(t); g != nil {
		g.recordSuccess()
	}
	r.clearModelFailure(t.Model.ModelName)
}

// RecordFailure penalises a provider group after an upstream failure.
func (r *Router) RecordFailure(t *Target, statusCode int) {
	g := r.find(t)
	if g == nil {
		return
	}
	for _, k := range g.keys {
		if k.apiKey == t.APIKey {
			g.recordFailure(k, statusCode)
			return
		}
	}
	g.recordFailure(g.keys[0], statusCode)
}

// RecordLatency feeds the rolling latency window used by health metrics.
func (r *Router) RecordLatency(t *Target, d time.Duration) {
	if g := r.find(t); g != nil {
		g.latency.record(d)
	}
}

// RecordModelFailure notes that every attempt for a model failed, starting a
// cooldown once the model has failed twice in a row.
func (r *Router) RecordModelFailure(modelName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelFailures[modelName]++
	if r.modelFailures[modelName] >= 2 {
		r.modelCooldown[modelName] = time.Now().Add(r.cooldownWindow)
	}
}

func (r *Router) clearModelFailure(modelName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.modelFailures, modelName)
	delete(r.modelCooldown, modelName)
}

func (r *Router) isCoolingDown(modelName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	expiry, ok := r.modelCooldown[modelName]
	if !ok {
		return false
	}
	if time.Now().Before(expiry) {
		return true
	}
	// Cooldown elapsed: clear it so the next resolve re-admits the model.
	delete(r.modelCooldown, modelName)
	delete(r.modelFailures, modelName)
	return false
}

func (r *Router) find(t *Target) groupRef {
	if t == nil {
		return nil
	}
	for _, g := range r.groups[t.Model.ModelName] {
		if g.slug == t.ProviderSlug && g.apiBase == t.APIBase {
			return g
		}
	}
	return nil
}

// GroupStatus is a point-in-time health snapshot for the admin API and routing
// graph in the dashboard.
type GroupStatus struct {
	ProviderSlug string  `json:"provider"`
	APIBase      string  `json:"api_base,omitempty"`
	Health       float64 `json:"health"`
	Breaker      string  `json:"breaker"`
	Errors       int     `json:"errors"`
	Requests     int     `json:"requests"`
	LatencyP50MS int64   `json:"latency_p50_ms"`
	LatencyP95MS int64   `json:"latency_p95_ms"`
	KeyCount     int     `json:"key_count"`
	KeysCooling  int     `json:"keys_cooling"`
}

// Status reports health for every group serving modelName.
func (r *Router) Status(modelName string) []GroupStatus {
	var out []GroupStatus
	for _, g := range r.groups[modelName] {
		g.mu.Lock()
		health, reqs, errs := g.health, g.requests, g.errors
		g.mu.Unlock()

		state, _ := g.breaker.snapshot()
		p50, p95 := g.latency.percentiles()

		cooling := 0
		now := time.Now()
		for _, k := range g.keys {
			k.mu.Lock()
			if k.cooldownUntil.After(now) {
				cooling++
			}
			k.mu.Unlock()
		}

		out = append(out, GroupStatus{
			ProviderSlug: g.slug,
			APIBase:      g.apiBase,
			Health:       health,
			Breaker:      state.String(),
			Errors:       errs,
			Requests:     reqs,
			LatencyP50MS: p50.Milliseconds(),
			LatencyP95MS: p95.Milliseconds(),
			KeyCount:     len(g.keys),
			KeysCooling:  cooling,
		})
	}
	return out
}
