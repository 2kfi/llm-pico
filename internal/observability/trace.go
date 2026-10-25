// Package observability provides in-memory request tracing and latency
// percentiles.
package observability

import (
	"sort"
	"sync"
	"time"
)

// Span is one timed stage of a request.
type Span struct {
	Label     string
	ModelName string
	Provider  string
	Status    string
	Detail    string
}

// Tracer accumulates the spans of a single in-flight request.
type Tracer struct {
	requestID string
	origin    time.Time

	mu    sync.Mutex
	spans []Span
}

// NewTracer starts tracing a request.
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

// ID returns the request identifier this tracer writes spans under. It is
// echoed to clients as X-Request-Id so a report can be traced back to spans.
func (t *Tracer) ID() string {
	if t == nil {
		return ""
	}
	return t.requestID
}

// LatencyTracker keeps a bounded rolling window of latencies per model and
// provider.
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

// Clear discards all samples.
func (t *LatencyTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.samples = map[string][]int{}
}
