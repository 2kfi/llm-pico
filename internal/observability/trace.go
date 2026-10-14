// Package observability provides request tracing, latency percentiles,
// a live event stream for the dashboard, and Prometheus metrics.
package observability

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/arkan/llm-pico/internal/store"
)

// Span is one timed stage of a request.
type Span struct {
	Label     string `json:"label"`
	StartMS   int64  `json:"start_ms"`
	EndMS     int64  `json:"end_ms"`
	ModelName string `json:"model_name"`
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
}

// Tracer accumulates the spans of a single in-flight request and persists them
// when the request finishes.
type Tracer struct {
	requestID string
	origin    time.Time

	mu    sync.Mutex
	spans []Span
}

// NewTracer starts tracing a request. Spans are timed relative to origin, so
// they line up with the dashboard waterfall view.
func NewTracer(requestID string) *Tracer {
	return &Tracer{requestID: requestID, origin: time.Now()}
}

// Add records a completed span. end may be zero to mean "now".
func (t *Tracer) Add(label, model, provider, status, detail string, end time.Time) {
	if t == nil {
		return
	}
	if end.IsZero() {
		end = time.Now()
	}
	span := Span{
		Label:     label,
		StartMS:   t.origin.UnixMilli(),
		EndMS:     end.UnixMilli(),
		ModelName: model,
		Provider:  provider,
		Status:    status,
		Detail:    detail,
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spans = append(t.spans, span)
}

// Spans returns a snapshot of the spans recorded so far.
func (t *Tracer) Spans() []Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Span, len(t.spans))
	copy(out, t.spans)
	return out
}

// Save writes the accumulated spans to the database. Tracing is best-effort:
// a failure here must never fail the request being traced.
func (t *Tracer) Save(ctx context.Context, db *store.Store) {
	if t == nil || db == nil {
		return
	}
	spans := t.Spans()
	if len(spans) == 0 {
		return
	}
	for i, s := range spans {
		_, err := db.Exec(ctx, `
			INSERT INTO request_traces
			  (request_id, span_index, label, start_ms, end_ms, model_name, provider, status, detail)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			t.requestID, i, s.Label, s.StartMS, s.EndMS, s.ModelName, s.Provider, s.Status, s.Detail)
		if err != nil {
			return
		}
	}
}

// Traces returns the spans of a completed request, ordered as recorded.
func Traces(ctx context.Context, db *store.Store, requestID string) ([]Span, error) {
	rows, err := db.Query(ctx, `
		SELECT label, start_ms, end_ms, model_name, provider, status, detail
		FROM request_traces WHERE request_id = ? ORDER BY span_index`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Span{}
	for rows.Next() {
		var s Span
		var model, provider, detail *string
		if err := rows.Scan(&s.Label, &s.StartMS, &s.EndMS, &model, &provider, &s.Status, &detail); err != nil {
			return nil, err
		}
		s.ModelName = deref(model)
		s.Provider = deref(provider)
		s.Detail = deref(detail)
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- latency profiling ---

// LatencyTracker keeps a bounded rolling window of latencies per model and
// provider, and reports percentiles for the dashboard and for slow-request
// detection.
type LatencyTracker struct {
	windowSize int

	mu      sync.Mutex
	samples map[string][]int
}

// NewLatencyTracker creates a tracker retaining the last windowSize samples
// per model/provider pair.
func NewLatencyTracker(windowSize int) *LatencyTracker {
	if windowSize <= 0 {
		windowSize = 1000
	}
	return &LatencyTracker{windowSize: windowSize, samples: map[string][]int{}}
}

// Record adds one observation.
func (t *LatencyTracker) Record(model, provider string, latencyMS int) {
	k := model + ":" + provider
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := t.samples[k]
	if len(buf) >= t.windowSize {
		buf = buf[len(buf)-t.windowSize+1:]
	}
	t.samples[k] = append(buf, latencyMS)
}

// Percentile returns the p-th percentile (0-100) across the window.
func (t *LatencyTracker) Percentile(model, provider string, p float64) int {
	k := model + ":" + provider
	t.mu.Lock()
	buf, ok := t.samples[k]
	t.mu.Unlock()
	if !ok || len(buf) == 0 {
		return 0
	}
	sorted := append([]int(nil), buf...)
	sort.Ints(sorted)
	idx := int(float64(len(sorted))*p/100) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// P50 returns the median latency.
func (t *LatencyTracker) P50(model, provider string) int { return t.Percentile(model, provider, 50) }

// P99 returns the 99th percentile latency.
func (t *LatencyTracker) P99(model, provider string) int { return t.Percentile(model, provider, 99) }

// IsSlow reports whether latencyMS is materially slower than this pair's own
// recent history. Comparing against the pair's baseline rather than a fixed
// threshold keeps the signal meaningful across providers with very different
// typical latencies.
func (t *LatencyTracker) IsSlow(model, provider string, latencyMS int) bool {
	p99 := t.P99(model, provider)
	if p99 == 0 {
		return false
	}
	return latencyMS > p99*3/2
}

// Clear discards all samples.
func (t *LatencyTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = map[string][]int{}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ID returns the request identifier this tracer writes spans under. It is also
// echoed to clients as X-Request-Id so a report can be traced back to spans.
func (t *Tracer) ID() string {
	if t == nil {
		return ""
	}
	return t.requestID
}
