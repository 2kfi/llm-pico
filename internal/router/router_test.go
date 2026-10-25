package router

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/arkan/llm-pico/internal/config"
)

// --- fixtures, mirroring python/tests/conftest.py ---

func baseConfig() *config.Config {
	return &config.Config{
		RouterSettings: config.RouterSettings{
			NumRetries:   2,
			CooldownTime: 45,
			CircuitBreaker: config.CircuitBreakerSettings{
				Enabled: true, FailureThreshold: 3, RecoveryTimeout: 30,
			},
		},
	}
}

func singleModelConfig() *config.Config {
	c := baseConfig()
	c.ModelList = []config.ModelEntry{{
		ModelName:   "test-model",
		ModelParams: config.ModelParams{Model: "openai/gpt-4", APIKey: "sk-test-key-1", APIBase: "https://api.openai.com/v1"},
	}}
	return c
}

func multiKeyConfig() *config.Config {
	c := baseConfig()
	c.ModelList = []config.ModelEntry{
		{
			ModelName:   "test-model",
			ModelParams: config.ModelParams{Model: "openai/gpt-4", APIKey: "sk-test-key-1", APIKeys: []string{"sk-test-key-1", "sk-test-key-2"}, APIBase: "https://api.openai.com/v1"},
		},
	}
	return c
}

func dualGroupConfig() *config.Config {
	c := baseConfig()
	c.ModelList = []config.ModelEntry{
		{
			ModelName:   "test-model",
			ModelParams: config.ModelParams{Model: "openai/gpt-4", APIKey: "sk-oa-key", APIBase: "https://api.openai.com/v1"},
		},
		{
			ModelName:   "test-model",
			ModelParams: config.ModelParams{Model: "groq/llama3", APIKey: "gsk-gr-key", APIBase: "https://api.groq.com/openai/v1"},
		},
	}
	return c
}

// --- TestResolve ---

func TestResolveReturnsGroupKeyAndEntry(t *testing.T) {
	r := New(singleModelConfig())
	got, err := r.Resolve("test-model")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ProviderSlug != "openai" {
		t.Errorf("slug = %q, want openai", got.ProviderSlug)
	}
	if got.APIKey != "sk-test-key-1" {
		t.Errorf("key = %q, want sk-test-key-1", got.APIKey)
	}
	if got.Model.ModelName != "test-model" {
		t.Errorf("entry = %q, want test-model", got.Model.ModelName)
	}
}

func TestResolveUnknownModel(t *testing.T) {
	r := New(singleModelConfig())
	if _, err := r.Resolve("nonexistent-model"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want ErrModelNotFound", err)
	}
}

func TestModelNames(t *testing.T) {
	r := New(singleModelConfig())
	names := r.ModelNames()
	if len(names) != 1 || names[0] != "test-model" {
		t.Errorf("names = %v, want [test-model]", names)
	}
}

func TestPicksNextKeyOnCooldown(t *testing.T) {
	r := New(multiKeyConfig())

	first, err := r.Resolve("test-model")
	if err != nil {
		t.Fatal(err)
	}

	// Cool down the returned key.
	r.RecordFailure(first, 429)

	second, err := r.Resolve("test-model")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if second.APIKey == first.APIKey {
		t.Errorf("second resolve returned the cooled-down key %q", second.APIKey)
	}
}

func TestResolveCooldownErrorWhenAllKeysCooled(t *testing.T) {
	r := New(multiKeyConfig())

	first, err := r.Resolve("test-model")
	if err != nil {
		t.Fatal(err)
	}
	r.RecordFailure(first, 429)

	second, err := r.Resolve("test-model")
	if err != nil {
		t.Fatal(err)
	}
	r.RecordFailure(second, 429)

	_, err = r.Resolve("test-model")
	var cd *CooldownError
	if !errors.As(err, &cd) {
		t.Fatalf("err = %v, want CooldownError", err)
	}
	if !errors.Is(err, ErrAllKeysCoolingDown) {
		t.Error("CooldownError should unwrap to ErrAllKeysCoolingDown")
	}
	if cd.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want > 0", cd.RetryAfter)
	}
}

func TestPicksOtherGroupWhenCircuitOpen(t *testing.T) {
	r := New(dualGroupConfig())

	// Trip the openai breaker directly; the weighted draw is random so we
	// cannot rely on resolve returning that group first.
	oa := r.groupBySlug("test-model", "openai")
	for i := 0; i < 3; i++ {
		oa.breaker.recordFailure()
	}
	if state, _ := oa.breaker.snapshot(); state != StateOpen {
		t.Fatalf("breaker state = %s, want OPEN", state)
	}

	got, err := r.Resolve("test-model")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ProviderSlug != "groq" {
		t.Errorf("slug = %q, want groq", got.ProviderSlug)
	}
	if got.APIKey != "gsk-gr-key" {
		t.Errorf("key = %q, want gsk-gr-key", got.APIKey)
	}
}

func TestCircuitBreakerRecoversAfterTimeout(t *testing.T) {
	r := New(dualGroupConfig())
	oa := r.groupBySlug("test-model", "openai")

	for i := 0; i < 3; i++ {
		oa.breaker.recordFailure()
	}
	if state, _ := oa.breaker.snapshot(); state != StateOpen {
		t.Fatalf("state = %s, want OPEN", state)
	}

	// Simulate the recovery timeout elapsing.
	oa.breaker.mu.Lock()
	oa.breaker.openedAt = time.Now().Add(-31 * time.Second)
	oa.breaker.mu.Unlock()

	if !oa.breaker.allows() {
		t.Fatal("breaker should admit a probe after the recovery timeout")
	}
	if state, _ := oa.breaker.snapshot(); state != StateHalfOpen {
		t.Errorf("state = %s, want HALF_OPEN", state)
	}

	oa.breaker.recordSuccess()
	state, errs := oa.breaker.snapshot()
	if state != StateClosed {
		t.Errorf("state = %s, want CLOSED", state)
	}
	if errs != 0 {
		t.Errorf("errorCount = %d, want 0", errs)
	}
}

// --- TestLatencyTracking ---

func TestRecordLatencyComputesPercentiles(t *testing.T) {
	g := &group{slug: "openai", latency: latencyWindow{max: 100}}
	for ms := 10; ms < 110; ms++ {
		g.latency.record(time.Duration(ms) * time.Millisecond)
	}
	p50, p95 := g.latency.percentiles()
	if got := float64(p50) / float64(time.Millisecond); math.Abs(got-59.5) > 0.001 {
		t.Errorf("p50 = %v ms, want 59.5", got)
	}
	if got := float64(p95) / float64(time.Millisecond); math.Abs(got-105) > 0.001 {
		t.Errorf("p95 = %v ms, want 105", got)
	}
}

func TestLatencySamplesCappedAt100(t *testing.T) {
	g := &group{slug: "openai", latency: latencyWindow{max: 100}}
	for i := 0; i < 150; i++ {
		g.latency.record(time.Duration(i) * time.Millisecond)
	}
	g.latency.mu.Lock()
	n := len(g.latency.samples)
	oldest := g.latency.samples[0]
	g.latency.mu.Unlock()

	if n != 100 {
		t.Errorf("sample count = %d, want 100", n)
	}
	if oldest != 50*time.Millisecond {
		t.Errorf("oldest sample = %v, want 50ms", oldest)
	}
}

func TestSingleLatencySample(t *testing.T) {
	g := &group{slug: "openai", latency: latencyWindow{max: 100}}
	g.latency.record(42 * time.Millisecond)
	p50, p95 := g.latency.percentiles()
	if p50 != 42*time.Millisecond || p95 != 42*time.Millisecond {
		t.Errorf("p50=%v p95=%v, want both 42ms", p50, p95)
	}
}

// --- TestWeightedRoundRobin ---

// The Python implementation weighted key selection by failure count. This port
// instead cools down failing keys and rotates round-robin across the remainder,
// which makes rotation deterministic and is what TestPicksNextKeyOnCooldown
// asserts.
func TestWeightedPickFavoursHealthyGroup(t *testing.T) {
	r := New(dualGroupConfig())

	// Degrade groq repeatedly; openai should win the weighted draw.
	for i := 0; i < 40; i++ {
		groq := &Target{
			ProviderSlug: "groq",
			APIBase:      "https://api.groq.com/openai/v1",
			APIKey:       "gsk-gr-key",
			Model:        config.ModelEntry{ModelName: "test-model"},
		}
		r.RecordFailure(groq, 500)
	}

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		got, err := r.Resolve("test-model")
		if err != nil {
			t.Fatal(err)
		}
		counts[got.ProviderSlug]++
	}
	if counts["openai"] <= counts["groq"] {
		t.Errorf("openai=%d groq=%d; healthy provider should dominate", counts["openai"], counts["groq"])
	}
}

func TestRoundRobinRotatesAcrossKeys(t *testing.T) {
	r := New(multiKeyConfig())

	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		got, err := r.Resolve("test-model")
		if err != nil {
			t.Fatal(err)
		}
		seen[got.APIKey]++
	}
	if len(seen) != 2 {
		t.Errorf("saw %d distinct keys, want 2: %v", len(seen), seen)
	}
	for k, n := range seen {
		if n != 5 {
			t.Errorf("key %s used %d times, want 5 (even round-robin)", k, n)
		}
	}
}

// --- model-level cooldown ---

func TestModelCooldownAfterRepeatedFailures(t *testing.T) {
	r := New(singleModelConfig())
	r.RecordModelFailure("test-model")
	if _, err := r.Resolve("test-model"); err != nil {
		t.Fatal("first failure should not trigger a cooldown")
	}

	r.RecordModelFailure("test-model")
	if _, err := r.Resolve("test-model"); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want ErrModelNotFound after two consecutive failures", err)
	}
}

func TestSuccessClearsModelCooldown(t *testing.T) {
	r := New(singleModelConfig())
	r.RecordModelFailure("test-model")
	r.RecordModelFailure("test-model")

	target, err := r.Resolve("test-model")
	if err == nil {
		t.Fatal("expected model to be cooling down")
	}
	_ = target

	// A success recorded by name clears the failure streak.
	r.RecordSuccess(&Target{Model: config.ModelEntry{ModelName: "test-model"}})
	if _, err := r.Resolve("test-model"); err != nil {
		t.Errorf("err = %v, want resolve to succeed after recovery", err)
	}
}

// --- concurrency ---

// The Python router mutated shared state from a single-threaded event loop, so
// races were invisible. Go runs requests on real goroutines, so the shared
// structures must be safe under concurrent resolution.
func TestConcurrentResolveIsRaceFree(t *testing.T) {
	r := New(dualGroupConfig())

	const workers = 32
	const iterations = 200

	done := make(chan struct{})
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < iterations; i++ {
				got, err := r.Resolve("test-model")
				if err != nil {
					continue
				}
				if i%2 == 0 {
					r.RecordSuccess(got)
					r.RecordLatency(got, time.Duration(i)*time.Millisecond)
				} else {
					r.RecordFailure(got, 429)
				}
				_ = r.Status("test-model")
			}
		}(w)
	}
	for w := 0; w < workers; w++ {
		<-done
	}
}

// --- helpers ---

func (r *Router) groupBySlug(model, slug string) *group {
	for _, g := range r.groups[model] {
		if g.slug == slug {
			return g
		}
	}
	return nil
}
