package proxy

import (
	"context"
	"fmt"
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

// Media parts are gated on the per-model flags before any upstream spend, and
// each kind is checked on its own: a model may allow video without images.
func TestMediaGateRejectsUnsupportedKind(t *testing.T) {
	const (
		imageBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`
		audioBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"a","format":"wav"}}]}]}`
		videoBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:video/mp4;base64,x"}}]}]}`
	)
	cases := []struct {
		name   string
		body   string
		image  bool
		audio  bool
		video  bool
		wantOK bool
	}{
		{"image denied", imageBody, false, false, false, false},
		{"audio denied", audioBody, false, false, false, false},
		{"video denied", videoBody, false, false, false, false},
		// Video is not image input: images off, video on must serve.
		{"video-only model", videoBody, false, false, true, true},
		{"audio allowed", audioBody, true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, hits := newUpstream(t)
			defer upstream.Close()
			cfg := chatConfig(upstream, "m", "a")
			entry := &cfg.ModelList[0]
			entry.Images, entry.Audio, entry.Video = tc.image, tc.audio, tc.video
			svc := newService(t, cfg, nil)

			_, err := svc.Dispatch(context.Background(), DispatchInput{
				Model: "m", RouteType: RouteChat, Body: []byte(tc.body),
			})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("dispatch: %v", err)
				}
				if len(hits()) != 1 {
					t.Fatalf("upstream hits = %d, want 1", len(hits()))
				}
				return
			}
			apiErr := httpx.FromError(err)
			if apiErr.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", apiErr.Status, apiErr.Message)
			}
			if len(hits()) != 0 {
				t.Error("upstream must not be reached for an unsupported media kind")
			}
		})
	}
}

// Media capability is a property of the slot, not of the model: a text-only
// slot ahead of a vision-capable one must be skipped rather than fail the
// request the caller could have served.
func TestMediaGateSkipsIncapableSlot(t *testing.T) {
	const imageBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`

	upstream, hits := newUpstream(t)
	defer upstream.Close()
	cfg := chatConfig(upstream, "m", "textonly", "vision")
	cfg.ModelList[1].Images = true
	svc := newService(t, cfg, nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(imageBody),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := hits(); len(got) != 1 || !strings.Contains(got[0], "key-vision") {
		t.Fatalf("hits = %v, want only the vision slot", got)
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

// An upstream 429 is the provider's wall, not the caller's: the walk must move
// to the next slot instead of refusing a request another provider can serve.
func TestUpstream429FallsOverToNextSlot(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		hits = append(hits, auth)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(auth, "key-a") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":5}}`))
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a", "b")
	cfg.ModelList[0].Retries = cfgInt(0)
	svc := newService(t, cfg, nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := resp.Header.Get("X-Pico-Provider"); got != "b" {
		t.Errorf("X-Pico-Provider = %q, want the slot that served", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 || !strings.Contains(hits[0], "key-a") || !strings.Contains(hits[1], "key-b") {
		t.Fatalf("hits = %v, want a then b", hits)
	}
}

// Upstream pacing metadata must reach the caller, attributed to the provider
// that produced it.
func TestUpstreamRateLimitMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a")
	cfg.ModelList[0].Retries = cfgInt(0)
	svc := newService(t, cfg, nil)

	_, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{}`),
	})
	apiErr := httpx.FromError(err)
	if apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", apiErr.Status)
	}
	if apiErr.Source != httpx.SourceUpstream {
		t.Errorf("source = %q, want upstream", apiErr.Source)
	}
	if apiErr.RetryAfter != 3 {
		t.Errorf("RetryAfter = %d, want the upstream's 3", apiErr.RetryAfter)
	}
	if got := apiErr.Headers.Get("X-Pico-Provider"); got != "a" {
		t.Errorf("X-Pico-Provider = %q, want a", got)
	}
	if got := apiErr.Headers.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want the upstream value", got)
	}
	if got := apiErr.Headers.Get("X-Pico-Attempts"); got != "1" {
		t.Errorf("X-Pico-Attempts = %q, want 1", got)
	}
}

// A per-model retries knob overrides the global one.
func TestModelRetriesOverrideGlobal(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a")
	cfg.RouterSettings.NumRetries = 5
	cfg.RouterSettings.CircuitBreaker.FailureThreshold = 100
	cfg.ModelList[0].Retries = cfgInt(1) // two tries, not six
	svc := newService(t, cfg, nil)

	_, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected the upstream failure to surface")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 from retries: 1", hits)
	}
}

// Prompt-cache hits bill at a fraction of the input rate, so the token window
// must charge only the uncached remainder.
func TestCachedTokensReconciled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{` +
			`"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,` +
			`"prompt_tokens_details":{"cached_tokens":90}}}`))
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "a")
	cfg.ModelList[0].TPM = cfgInt(1000)
	lim := ratelimit.New()
	defer lim.Stop()
	svc := newService(t, cfg, lim)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	if got := lim.Usage("", "m", "a", ratelimit.LevelModel, ratelimit.TPM); got != 20 {
		t.Fatalf("TPM usage = %d, want 20 (110 total minus 90 cached)", got)
	}
}

// A stalled upstream must fail the stream after the idle deadline instead of
// hanging the agent forever.
func TestStreamIdleWatchdogTearsDownStalledStream(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer upstream.Close()

	cfg := chatConfig(upstream, "m", "alpha")
	cfg.RouterSettings.StreamIdleTimeout = 1 // one second
	svc := newService(t, cfg, nil)

	resp, err := svc.Dispatch(context.Background(), DispatchInput{
		Model: "m", RouteType: RouteChat, Body: []byte(`{"model":"m","stream":true}`),
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled stream must surface an error")
		}
		if !strings.Contains(err.Error(), "stalled") {
			t.Errorf("error = %v, want it to name the stall", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("stream did not time out; the watchdog never fired")
	}
	close(release)
}

// The estimator must land within a fifth of the real token count on a large
// tool-loop body, rather than charging the raw JSON byte count.
func TestEstimateTokensOnToolLoopBody(t *testing.T) {
	const textLen = 25 * 1024
	messages := make([]string, 0, 45)
	for i := 0; i < 45; i++ {
		content := strings.Repeat("abcde ", textLen/6)
		messages = append(messages, fmt.Sprintf(
			`{"role":"tool","tool_call_id":"call_%d","content":%q}`, i, content))
	}
	body := []byte(`{"model":"m","messages":[` + strings.Join(messages, ",") + `]}`)
	if len(body) < 1<<20 {
		t.Fatalf("fixture is %d bytes, want about 1 MB", len(body))
	}

	got := EstimateBodyTokens(body)
	want := int64(45 * textLen / 4)
	if got < want*8/10 || got > want*12/10 {
		t.Fatalf("estimate = %d, want within 20%% of %d", got, want)
	}
}

// Base64 media is not text: an inline image must not be charged a quarter of
// a million tokens for its bytes.
func TestEstimateTokensIgnoresMediaBulk(t *testing.T) {
	data := strings.Repeat("A", 1<<20)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"describe this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + data + `"}}]}]}`)

	got := EstimateBodyTokens(body)
	if got > 5000 {
		t.Fatalf("estimate = %d for a 1 MB inline image, want a flat media band", got)
	}
}
