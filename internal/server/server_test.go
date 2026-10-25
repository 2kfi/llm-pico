package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
