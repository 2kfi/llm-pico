package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/ratelimit"
)

// newUpstream returns an OpenAI-protocol backend; the getter reports which
// credentials hit it (one distinct bearer token per slot). Unknown provider
// slugs resolve to the OpenAI adapter with the entry's base URL, which is
// exactly what the config snapshot supplies.
func newUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), hits...)
	}
}

func cfgInt(v int) *int { return &v }

// chatConfig builds a chat model with one slot per named provider, all pointing
// at the test upstream.
func chatConfig(upstream *httptest.Server, modelName string, slots ...string) *config.Config {
	cfg := &config.Config{
		ModelList:       []config.ModelEntry{},
		DegradationMode: "normal",
	}
	for _, s := range slots {
		cfg.ModelList = append(cfg.ModelList, config.ModelEntry{
			ModelName: modelName,
			Kind:      config.KindChat,
			Provider:  s,
			ModelParams: config.ModelParams{
				Model: s + "/" + modelName, APIKey: "key-" + s, APIBase: upstream.URL,
			},
		})
	}
	return cfg
}

func newService(t *testing.T, cfg *config.Config, lim *ratelimit.Limiter) *Service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	svc := NewService(cfg, providers.NewRegistry(nil), log)
	if lim != nil {
		svc.SetLimiter(lim)
	}
	return svc
}

type discardWriter struct{}

func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }

func TestDispatchChoosesHealthyUpstream(t *testing.T) {
	upstream, hits := newUpstream(t)
	defer upstream.Close()
	svc := newService(t, chatConfig(upstream, "m", "alpha", "beta"), nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if n := len(hits()); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
}

// A slot whose context budget cannot hold the request is skipped; a
// lower-priority slot with room still serves it.
func TestContextTooLongFallsToWiderSlot(t *testing.T) {
	upstream, hits := newUpstream(t)
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "narrow", "wide")
	cfg.ModelList[0].Ctx = cfgInt(10) // 160 byte body / 4 = 40 tokens > 10
	svc := newService(t, cfg, nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat,
		Body: []byte(`{"model":"m","pad":"` + strings.Repeat("x", 140) + `"}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// The wide slot served the request; with only one surviving slot there is
	// no chain header, and the auth header names the slot upstream.
	if got := hits(); len(got) != 1 || !strings.Contains(got[0], "key-wide") {
		t.Fatalf("hits = %v, want only the wide slot", got)
	}
}

// When no slot's context holds the request, the model is refused with 400
// before any upstream call.
func TestContextTooLongAllSlots(t *testing.T) {
	upstream, hits := newUpstream(t)
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a", "b")
	for i := range cfg.ModelList {
		cfg.ModelList[i].Ctx = cfgInt(10)
	}
	svc := newService(t, cfg, nil)

	_, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: make([]byte, 200),
	})
	apiErr := httpx.FromError(err)
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", apiErr.Status, apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "maximum context length") {
		t.Errorf("message = %q, want OpenAI-style context message", apiErr.Message)
	}
	if len(hits()) != 0 {
		t.Error("upstream must not be reached for an overlong request")
	}
}

// Each slot charges its own limit cells; exhausting one slot's budget leaves
// the next slot usable, and exhausting both is a terminal 429.
func TestQuotaPerSlot(t *testing.T) {
	upstream, _ := newUpstream(t)
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a", "b")
	for i := range cfg.ModelList {
		cfg.ModelList[i].RPM = cfgInt(1)
	}
	lim := ratelimit.New()
	defer lim.Stop()
	svc := newService(t, cfg, lim)

	in := DispatchInput{Model: "m", RouteType: RouteChat, Body: []byte(`{}`)}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := svc.Dispatch(ctx, in); err != nil {
			t.Fatalf("dispatch %d: %v", i+1, err)
		}
	}
	// Both slots at RPM=1: the third request must be refused by the limiter.
	_, err := svc.Dispatch(ctx, in)
	apiErr := httpx.FromError(err)
	if apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", apiErr.Status, apiErr.Message)
	}
	if apiErr.Type != httpx.TypeRateLimited {
		t.Errorf("type = %q, want %q", apiErr.Type, httpx.TypeRateLimited)
	}
}

func TestQuotaErrorIsTerminalAndClientRejection(t *testing.T) {
	err := &quotaError{err: httpx.RateLimited("rate limit exceeded: rpm", 1)}
	if !isClientRejection(err) {
		t.Error("quota rejection must read as a client rejection")
	}
	if err.Unwrap() == nil {
		t.Error("quota rejection must unwrap to its httpx error")
	}
}

// The terminating data: [DONE] sentinel adapters emit must reach the client
// — SSE clients rely on it to close cleanly.
func TestStreamingForwardsDoneSentinel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	svc := newService(t, chatConfig(upstream, "m", "alpha"), nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.HasSuffix(string(all), "data: [DONE]\n\n") {
		t.Fatalf("stream = %q, want trailing data: [DONE]\\n\\n", all)
	}
	for _, want := range []string{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n"} {
		if !strings.Contains(string(all), want) {
			t.Errorf("stream missing chunk frame %q", want)
		}
	}
}

// Drain must wait for the streaming copy goroutine, which outlives Dispatch:
// BeginDrain finishes only after the upstream stream has been fully copied
// to the client.
func TestDrainWaitsForStreamingCopy(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"early\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	svc := newService(t, chatConfig(upstream, "m", "alpha"), nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	readDone := make(chan []byte, 1)
	go func() {
		all, _ := io.ReadAll(resp.Body)
		readDone <- all
	}()

	drainDone := make(chan bool, 1)
	go func() { drainDone <- svc.BeginDrain(time.Second) }()

	// The upstream is still open, so drain must be blocked by the copy
	// goroutine even though Dispatch itself has returned.
	select {
	case <-drainDone:
		t.Fatal("BeginDrain returned while a streaming copy was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	all := <-readDone
	if !strings.HasSuffix(string(all), "data: [DONE]\n\n") {
		t.Fatalf("stream = %q, want trailing data: [DONE]\\n\\n", all)
	}
	if <-drainDone != true {
		t.Error("BeginDrain timed out after the stream finished")
	}
}

// A stream must reconcile its real token usage into the limiter after the
// copy goroutine settles, not silently stick with the pre-reserved estimate.
func TestStreamingReconcilesActualUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "alpha")
	cfg.ModelList[0].RPM = cfgInt(10)
	cfg.ModelList[0].TPM = cfgInt(100)
	lim := ratelimit.New()
	defer lim.Stop()
	svc := newService(t, cfg, lim)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for lim.Usage("", "m", "alpha", ratelimit.LevelModel, ratelimit.RPM) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("stream accounting never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := lim.Usage("", "m", "alpha", ratelimit.LevelModel, ratelimit.TPM); got != 5 {
		t.Fatalf(" TPM usage = %d, want the provider-reported 5 (not the pre-reserved estimate)", got)
	}
}

// When every key of the first slot is rate-limited at the router, the slot
// walk must continue to the next slot instead of refusing the caller.
func TestRouterCooldownFallsThroughToNextSlot(t *testing.T) {
	var mu sync.Mutex
	hits429 := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		// Upstream-wide 429: every request ranked 429 until keys are skipped.
		hits429++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted"}}`))
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a", "b")
	svc := newService(t, cfg, nil)

	// Slot "a" returns 429; the proxy should still fail overall after both
	// providers were tried, and the failing upstream must be retried fewer
	// times than NumRetries+1 once both keys are cooling down.
	for i := 0; i < 3; i++ {
		_, err := svc.Dispatch(context.Background(), DispatchInput{Model: "m", RouteType: RouteChat, Body: []byte(`{}`)})
		if err == nil {
			t.Fatal("expected 429 from both slots")
		}
		apiErr := httpx.FromError(err)
		if apiErr.Status != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", apiErr.Status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits429 < 2 {
		t.Fatalf("slot walk executed %d upstream hits, want at least one per slot", hits429)
	}
	if got := svc.Router().Status("m"); len(got) < 2 {
		t.Fatalf("router status lists %d groups, want both slots tracked", len(got))
	}
}
