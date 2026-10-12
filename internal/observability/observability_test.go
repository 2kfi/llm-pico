package observability

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkan/llm-pico/internal/store"
)

// --- latency tracker ---

func TestLatencyPercentiles(t *testing.T) {
	tr := NewLatencyTracker(1000)
	for i := 10; i < 110; i++ {
		tr.Record("gpt-4", "openai", i)
	}
	if got := tr.P50("gpt-4", "openai"); got != 59 {
		t.Errorf("p50 = %d, want 59", got)
	}
	// Percentiles use the conventional sorted-index definition
	// idx = floor(n*p/100) - 1, which for n=100 lands on element 98.
	if got := tr.P99("gpt-4", "openai"); got != 108 {
		t.Errorf("p99 = %d, want 108", got)
	}
}

func TestLatencyEmptyWindow(t *testing.T) {
	tr := NewLatencyTracker(1000)
	if got := tr.P50("x", "y"); got != 0 {
		t.Errorf("p50 = %d, want 0 for an empty window", got)
	}
	if got := tr.P99("x", "y"); got != 0 {
		t.Errorf("p99 = %d, want 0 for an empty window", got)
	}
	if tr.IsSlow("x", "y", 500) {
		t.Error("IsSlow should be false with no baseline")
	}
}

func TestLatencyWindowEviction(t *testing.T) {
	tr := NewLatencyTracker(10)
	for i := 0; i < 25; i++ {
		tr.Record("m", "p", i)
	}
	tr.mu.Lock()
	n := len(tr.samples["m:p"])
	first := tr.samples["m:p"][0]
	tr.mu.Unlock()

	if n != 10 {
		t.Errorf("window size = %d, want 10", n)
	}
	if first != 15 {
		t.Errorf("oldest sample = %d, want 15", first)
	}
}

func TestIsSlowComparesAgainstOwnBaseline(t *testing.T) {
	tr := NewLatencyTracker(1000)
	for i := 0; i < 100; i++ {
		tr.Record("m", "p", 100)
	}
	if tr.IsSlow("m", "p", 150) {
		t.Error("150ms is within the 1.5x baseline and should not be slow")
	}
	if !tr.IsSlow("m", "p", 400) {
		t.Error("400ms is 4x the baseline and should be slow")
	}
}

func TestLatencyClear(t *testing.T) {
	tr := NewLatencyTracker(100)
	tr.Record("m", "p", 5)
	tr.Clear()
	if got := tr.P50("m", "p"); got != 0 {
		t.Errorf("p50 after Clear = %d, want 0", got)
	}
}

func TestLatencyTrackerConcurrent(t *testing.T) {
	tr := NewLatencyTracker(500)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tr.Record("m", "p", j)
				_ = tr.P50("m", "p")
			}
		}()
	}
	wg.Wait()
}

// --- event bus ---

func TestBusDeliversToSubscriber(t *testing.T) {
	bus := NewBus(8)
	ch, cancel := bus.Subscribe()
	defer cancel()

	bus.Publish(NewEvent("sk-pico-a", "gpt-4", "openai", 10, 20, 30, 150, 200, 0.01))

	select {
	case payload := <-ch:
		if !strings.Contains(string(payload), `"model":"gpt-4"`) {
			t.Errorf("payload = %s, want the model field", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestBusMultipleSubscribers(t *testing.T) {
	bus := NewBus(8)
	a, cancelA := bus.Subscribe()
	b, cancelB := bus.Subscribe()
	defer cancelA()
	defer cancelB()

	if bus.Subscribers() != 2 {
		t.Errorf("subscribers = %d, want 2", bus.Subscribers())
	}
	bus.Publish(NewEvent("k", "m", "p", 1, 1, 2, 10, 200, 0))

	for i, ch := range []<-chan []byte{a, b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

// A dashboard that stops reading must not block the request path or grow
// memory without bound.
func TestBusDropsOldestWhenSubscriberIsSlow(t *testing.T) {
	bus := NewBus(4)
	ch, cancel := bus.Subscribe()
	defer cancel()

	for i := 0; i < 20; i++ {
		bus.Publish(NewEvent("k", "m", "p", i, 0, i, 1, 200, 0))
	}

	if len(ch) != 4 {
		t.Errorf("buffer length = %d, want it capped at 4", len(ch))
	}
	// The newest event must be the one retained.
	last := <-ch
	<-ch
	<-ch
	newest := <-ch
	if !strings.Contains(string(newest), `"prompt_tokens":19`) {
		t.Errorf("retained event = %s, want the newest (prompt_tokens 19)", newest)
	}
	_ = last
}

func TestBusUnsubscribe(t *testing.T) {
	bus := NewBus(8)
	_, cancel := bus.Subscribe()
	if bus.Subscribers() != 1 {
		t.Fatal("expected one subscriber")
	}
	cancel()
	if bus.Subscribers() != 0 {
		t.Error("Unsubscribe did not remove the subscriber")
	}
	// Publishing after unsubscribe must not panic on a closed channel.
	bus.Publish(NewEvent("k", "m", "p", 1, 1, 2, 1, 200, 0))
}

func TestBusConcurrentPublishAndSubscribe(t *testing.T) {
	bus := NewBus(16)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				bus.Publish(NewEvent("k", "m", "p", 1, 1, 2, 1, 200, 0))
			}
		}
	}()

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancel := bus.Subscribe()
			time.Sleep(time.Millisecond)
			cancel()
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- tracing ---

func TestTracerCollectsSpans(t *testing.T) {
	tr := NewTracer("req-1")
	tr.Add("resolve:gpt-4", "gpt-4", "openai", "ok", "", time.Time{})
	tr.Add("proxy:gpt-4", "gpt-4", "openai", "ok", "", time.Time{})

	spans := tr.Spans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if spans[0].Label != "resolve:gpt-4" {
		t.Errorf("first label = %q", spans[0].Label)
	}
	if spans[0].EndMS < spans[0].StartMS {
		t.Error("span ends before it starts")
	}
}

func TestTracerSaveAndLoad(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	tr := NewTracer("req-2")
	tr.Add("resolve:gpt-4", "gpt-4", "openai", "ok", "", time.Time{})
	tr.Add("error:gpt-4", "gpt-4", "openai", "fail", "502", time.Time{})
	tr.Save(ctx, db)

	spans, err := Traces(ctx, db, "req-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("loaded %d spans, want 2", len(spans))
	}
	if spans[0].Label != "resolve:gpt-4" || spans[1].Label != "error:gpt-4" {
		t.Errorf("spans out of order: %+v", spans)
	}
	if spans[1].Status != "fail" || spans[1].Detail != "502" {
		t.Errorf("second span = %+v, want the failure recorded", spans[1])
	}
}

func TestTracerSaveIsSafeWhenEmpty(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	NewTracer("req-3").Save(context.Background(), db)
	var n int
	if err := db.Get(context.Background(), &n, `SELECT COUNT(*) FROM request_traces`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("wrote %d spans, want 0", n)
	}
}

// --- metrics ---

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPrometheusRendersCounters(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	cost := 0.5
	for _, r := range []struct {
		model, provider string
		status          int
		tokens          int
	}{
		{"gpt-4", "openai", 200, 100},
		{"gpt-4", "openai", 200, 50},
		{"gpt-4", "openai", 500, 10},
		{"claude", "anthropic", 200, 75},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO usage_log
			  (key_hash, key_prefix, model_name, provider, request_id,
			   prompt_tokens, completion_tokens, total_tokens,
			   latency_ms, status_code, cost_usd, error_type, created_at)
			VALUES ('h','sk-pico-a',?,?,'r',1,1,?,10,?,?,?,?)`,
			r.model, r.provider, r.tokens, r.status, &cost,
			errorTypeFor(r.status), store.Now()); err != nil {
			t.Fatal(err)
		}
	}

	out, err := Prometheus(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"# HELP llm_pico_requests_total",
		"# TYPE llm_pico_requests_total counter",
		`llm_pico_requests_total{model="gpt-4",provider="openai",status="200"} 2`,
		`llm_pico_requests_total{model="gpt-4",provider="openai",status="500"} 1`,
		`llm_pico_tokens_total{model="gpt-4",provider="openai"} 160`,
		`llm_pico_cost_usd_total{model="gpt-4",provider="openai"} 1.5`,
		`llm_pico_errors_total{type="upstream_error"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, out)
		}
	}
}

func TestPrometheusEmptyDatabase(t *testing.T) {
	out, err := Prometheus(context.Background(), newTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	// Headers must still be emitted so scrapers see the series as declared.
	if !strings.Contains(out, "# TYPE llm_pico_requests_total counter") {
		t.Errorf("expected HELP/TYPE headers on an empty database, got:\n%s", out)
	}
	if strings.Contains(out, "llm_pico_requests_total{") {
		t.Errorf("expected no samples on an empty database, got:\n%s", out)
	}
}

func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		`plain`:     `"plain"`,
		`with"q`:    `"with\"q"`,
		`back\s`:    `"back\\s"`,
		"new\nline": `"new\nline"`,
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFormatFloat(t *testing.T) {
	if got := formatFloat(0); got != "0" {
		t.Errorf("formatFloat(0) = %q, want 0", got)
	}
	if got := formatFloat(1.5); got != "1.5" {
		t.Errorf("formatFloat(1.5) = %q, want 1.5", got)
	}
	if got := formatFloat(-0.0); got != "0" {
		t.Errorf("formatFloat(-0.0) = %q, want 0", got)
	}
}

func errorTypeFor(status int) string {
	switch status {
	case 500:
		return "upstream_error"
	case 429:
		return "rate_limit"
	default:
		return ""
	}
}
