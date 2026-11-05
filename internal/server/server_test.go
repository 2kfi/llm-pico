package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/proxy"
)

// testConfig builds a single-model configuration pointing at an
// OpenAI-shaped test upstream, with the given proxy and HMAC settings.
func testConfig(upstream *httptest.Server, hmacEnabled bool) *config.Config {
	return &config.Config{
		ProxyKeys: []string{"sk-test"},
		GeneralSettings: config.GeneralSettings{
			HMACEnabled: hmacEnabled,
			HMACSecret:  "s3cret",
		},
		ModelList: []config.ModelEntry{{
			ModelName: "m",
			Kind:      config.KindChat,
			Provider:  "alpha",
			ModelParams: config.ModelParams{
				Model: "alpha/m", APIKey: "key-alpha", APIBase: upstream.URL,
			},
		}},
	}
}

// newTestServer wires a Server against an OpenAI-shaped test upstream.
func newTestServer(t *testing.T, hmacEnabled bool) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":5}}`))
	}))
	t.Cleanup(upstream.Close)

	svc := proxy.NewService(testConfig(upstream, hmacEnabled), providers.NewRegistry(nil), slog.New(slog.DiscardHandler))
	return New(serverOptions(t, svc))
}

func serverOptions(t *testing.T, svc *proxy.Service) Options {
	return Options{
		Proxy:        svc,
		Logger:       slog.New(slog.DiscardHandler),
		MaxBodyBytes: 4096,
		DrainTimeout: 1,
	}
}

func signedRequest(body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func plainRequest(body []byte) *http.Request {
	return requestTo("POST", "/v1/chat/completions", body)
}

func requestTo(method, path string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-test")
	return r
}

// A body over the JSON route limit must be rejected before dispatch.
func TestOversizedBodyRejected(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer(t, false).ServeHTTP(w, plainRequest(bytes.Repeat([]byte("x"), 8<<20)))
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(httpx.TypeBadRequest)) {
		t.Fatalf("status = %d, body = %s, want a 400 rejection", w.Code, w.Body.String())
	}
}

// An oversized body signed for the HMAC path hits the same bound: auth sees
// the already-bounded bytes, so it cannot drain an unbounded stream first.
func TestOversizedBodyRejectedWithHMAC(t *testing.T) {
	w := httptest.NewRecorder()
	newTestServer(t, true).ServeHTTP(w, signedRequest(bytes.Repeat([]byte("x"), 8<<20)))
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(httpx.TypeBadRequest)) {
		t.Fatalf("status = %d, body = %s, want a 400 rejection", w.Code, w.Body.String())
	}
}

// An unauthenticated caller must be rejected without the body being read:
// an oversized body with an invalid key gets 401, not the size rejection,
// so auth cannot be made to buffer untrusted input.
func TestUnauthenticatedBodyNotBuffered(t *testing.T) {
	r := requestTo("POST", "/v1/chat/completions", bytes.Repeat([]byte("x"), 8<<20))
	r.Header.Set("Authorization", "Bearer sk-wrong")
	w := httptest.NewRecorder()
	newTestServer(t, false).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s, want 401 before any body read", w.Code, w.Body.String())
	}
}

// A valid HMAC signature over a body exactly at the limit is accepted: the
// reader must allow len == limit and fail only when it is exceeded.
func TestHMACBodyAtLimitAccepted(t *testing.T) {
	// The JSON body is exactly at the 4 KiB limit and carries the model
	// field near its start so the upstream dispatch succeeds; a 200 proves
	// the body survived both the bound and the signature check.
	body := make([]byte, 0, 4096)
	body = append(body, `{"model":"m","pad":"x`...)
	for len(body) < 4096-3 {
		body = append(body, 'x')
	}
	body = append(body, []byte(`"}`)...)
	body = append(body, '\n')
	if len(body) != 4096 {
		t.Fatalf("test body length = %d, want 4096", len(body))
	}

	w := httptest.NewRecorder()
	newTestServer(t, true).ServeHTTP(w, signedRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
}

// A slot without dims must not prevent a later slot's dims from being
// enforced: a request for the wrong size is refused.
// The proxy rewrites adapter bodies, so upstream framing headers must not
// describe the bytes sent to the caller.
func TestCopyResponseHeadersDropsFraming(t *testing.T) {
	src := http.Header{
		"Content-Length":        {"245"},
		"Transfer-Encoding":     {"chunked"},
		"Connection":            {"keep-alive"},
		"Content-Type":          {"application/json"},
		"X-RateLimit-Remaining": {"7"},
	}
	dst := http.Header{}
	copyResponseHeaders(dst, src)
	for _, key := range []string{"Content-Length", "Transfer-Encoding", "Connection"} {
		if got := dst.Get(key); got != "" {
			t.Errorf("%s = %q, want it dropped", key, got)
		}
	}
	if dst.Get("Content-Type") != "application/json" || dst.Get("X-RateLimit-Remaining") != "7" {
		t.Errorf("end-to-end headers lost: %v", dst)
	}
}

// /v1/models advertises the capabilities the media gate and reasoning controls
// honour, unioned across a model's slots.
func TestModelListingExposesCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream, false)
	cfg.ModelList[0].Audio = true
	cfg.ModelList[0].Thinking = true
	// A second slot contributes image support, so the union advertises it.
	cfg.ModelList = append(cfg.ModelList, config.ModelEntry{
		ModelName:   "m",
		Kind:        config.KindChat,
		Provider:    "beta",
		ModelParams: config.ModelParams{Model: "beta/m", APIKey: "key-beta", APIBase: upstream.URL},
		Images:      true,
	})

	svc := proxy.NewService(cfg, providers.NewRegistry(nil), slog.New(slog.DiscardHandler))
	srv := New(serverOptions(t, svc))

	list := httptest.NewRecorder()
	srv.ServeHTTP(list, requestTo("GET", "/v1/models", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", list.Code, list.Body.String())
	}
	var listing struct {
		Data []struct {
			ID               string `json:"id"`
			SupportsImages   bool   `json:"supports_images"`
			SupportsAudio    bool   `json:"supports_audio"`
			SupportsVideo    bool   `json:"supports_video"`
			SupportsThinking bool   `json:"supports_thinking"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Data) != 1 || listing.Data[0].ID != "m" {
		t.Fatalf("listing = %+v, want one model named m", listing.Data)
	}
	got := listing.Data[0]
	if !got.SupportsImages || !got.SupportsAudio || got.SupportsVideo || !got.SupportsThinking {
		t.Errorf("capabilities = images:%v audio:%v video:%v thinking:%v, want true/true/false/true",
			got.SupportsImages, got.SupportsAudio, got.SupportsVideo, got.SupportsThinking)
	}

	single := httptest.NewRecorder()
	srv.ServeHTTP(single, requestTo("GET", "/v1/models/m", nil))
	if single.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", single.Code, single.Body.String())
	}
	var one struct {
		ID               string `json:"id"`
		SupportsThinking bool   `json:"supports_thinking"`
	}
	if err := json.Unmarshal(single.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.ID != "m" || !one.SupportsThinking {
		t.Errorf("single model = %+v, want m with thinking support", one)
	}
}

func TestStatusEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream, false)
	svc := proxy.NewService(cfg, providers.NewRegistry(nil), slog.New(slog.DiscardHandler))
	srv := New(serverOptions(t, svc))

	// Record a failure so the status shows something interesting.
	target, _ := svc.Router().Resolve("m")
	svc.Router().RecordFailure(target, 429)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, requestTo("GET", "/v1/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var status map[string][]struct {
		Provider string `json:"provider"`
		Health   float64 `json:"health"`
		Keys     []struct {
			Prefix string `json:"prefix"`
			Status string `json:"status"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status["m"]) == 0 {
		t.Fatal("status for model 'm' missing")
	}
	group := status["m"][0]
	if group.Health >= 1.0 {
		t.Errorf("health = %f, want < 1.0 after failure", group.Health)
	}
	if len(group.Keys) == 0 || group.Keys[0].Status != "COOLING_DOWN" {
		t.Errorf("key status = %v, want COOLING_DOWN", group.Keys)
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer(t, false)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want ok", resp["status"])
	}
}

func TestCheckDimsHonoursLaterSlot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		ProxyKeys: []string{"sk-test"},
		ModelList: []config.ModelEntry{
			{ModelName: "m", Kind: config.KindEmbeddings, Provider: "alpha",
				ModelParams: config.ModelParams{Model: "e", APIKey: "k", APIBase: upstream.URL}},
			{ModelName: "m", Kind: config.KindEmbeddings, Provider: "alpha", Dims: intPtr(1536),
				ModelParams: config.ModelParams{Model: "e", APIKey: "k", APIBase: upstream.URL}},
		},
	}
	svc := proxy.NewService(cfg, providers.NewRegistry(nil), slog.New(slog.DiscardHandler))
	srv := New(serverOptions(t, svc))

	requestDims := func(n int) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, requestTo("POST", "/v1/embeddings", []byte(`{"model":"m","input":"x","dimensions":`+itos(n)+`}`)))
		return w
	}

	if got := requestDims(512); got.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400 for mismatched dimensions", got.Code, got.Body.String())
	}
	if got := requestDims(1536); got.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 for matching dimensions", got.Code, got.Body.String())
	}
}

func intPtr(n int) *int { return &n }

func itos(n int) string { return strconv.Itoa(n) }
