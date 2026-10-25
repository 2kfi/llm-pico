// Package server exposes the proxy over the OpenAI-compatible endpoints.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/arkan/llm-pico/internal/auth"
	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/proxy"
	"github.com/arkan/llm-pico/internal/ratelimit"
)

// Options configures a Server.
type Options struct {
	Proxy  *proxy.Service
	Logger *slog.Logger
	// MaxBodyBytes bounds request bodies on JSON routes. Audio uploads
	// have their own, larger limit because a shared 64 MiB bound would
	// refuse long recordings.
	MaxBodyBytes int64
	// DrainTimeout bounds how long shutdown waits for in-flight requests.
	DrainTimeout time.Duration
}

// Server owns the HTTP surface.
type Server struct {
	proxy   *proxy.Service
	log     *slog.Logger
	limiter *ratelimit.Limiter
	mux     *http.ServeMux
	opts    Options
}

// New builds a Server with all routes registered.
func New(opts Options) *Server {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 64 << 20 // 64 MiB
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 30 * time.Second
	}
	s := &Server{
		proxy:   opts.Proxy,
		log:     opts.Logger,
		limiter: opts.Proxy.Limiter(),
		opts:    opts,
	}
	s.mux = s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	s.mux.ServeHTTP(w, r)
}

// routes registers every endpoint.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// OpenAI-compatible surface.
	mux.HandleFunc("POST /v1/chat/completions", s.handle(proxy.RouteChat))
	mux.HandleFunc("POST /v1/completions", s.handle(proxy.RouteCompletions))
	mux.HandleFunc("POST /v1/embeddings", s.handle(proxy.RouteEmbeddings))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handle(proxy.RouteSTT))
	mux.HandleFunc("POST /v1/audio/speech", s.handle(proxy.RouteTTS))
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("GET /v1/models/{id}", s.getModel)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version})
	})

	return mux
}

// handle builds a proxy handler for one route type.
func (s *Server) handle(route proxy.RouteType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Audio uploads are the largest legitimate payload, so they get
		// their own ceiling; every other route shares the general bound.
		// The limit is installed before anything reads the body, so auth
		// cannot consume an unbounded stream.
		limit := s.opts.MaxBodyBytes
		if route == proxy.RouteSTT {
			limit = maxAudioBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		body, err := readBody(r)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				httpx.BadRequest("request body too large").Write(w)
				return
			}
			httpx.BadRequest("could not read request body").Write(w)
			return
		}

		identity, authErr := s.authenticate(w, r, body)
		if authErr != nil {
			authErr.Write(w)
			return
		}

		input := proxy.DispatchInput{
			Body:      body,
			Identity:  identity,
			RouteType: route,
		}

		switch route {
		case proxy.RouteSTT:
			// The multipart parser needs a live reader; the body has
			// already been bounded and read above.
			r.Body = io.NopCloser(bytes.NewReader(body))
			audio, fields, apiErr := s.parseAudio(r)
			if apiErr != nil {
				apiErr.Write(w)
				return
			}
			input.Audio = audio
			input.Model = fields["model"]
			// ~32 kB of compressed audio per second is the usual bitrate
			// band; the estimate only feeds the audio-seconds limits.
			input.AudioSeconds = max(1, len(audio.Data)/32000)
			input.MaxTokens = 0
		case proxy.RouteTTS:
			if !json.Valid(body) {
				httpx.BadRequest("invalid JSON body").Write(w)
				return
			}
			input.Model = peekString(body, "model")
			input.Chars = len(peekString(body, "input"))
			// Spoken cadence is roughly fifteen characters per second.
			input.AudioSeconds = (input.Chars + 14) / 15
			input.MaxTokens = 0
		default:
			// A body that is not a JSON object cannot be dispatched, and
			// reporting that plainly beats reporting a missing model field,
			// which is what peeking would otherwise produce.
			if !json.Valid(body) {
				httpx.BadRequest("invalid JSON body").Write(w)
				return
			}
			input.Model = peekString(body, "model")
			input.MaxTokens = peekInt(body, "max_tokens", 4096)
		}

		if input.Model == "" {
			httpx.BadRequest("field 'model' is required").Write(w)
			return
		}

		cfg := s.proxy.Config()
		canonical := cfg.Canonical(input.Model)
		if len(cfg.Slots(canonical)) == 0 {
			httpx.NotFound(fmt.Sprintf("model '%s' not available", input.Model)).Write(w)
			return
		}

		if route == proxy.RouteEmbeddings {
			if apiErr := s.checkDims(body, canonical); apiErr != nil {
				apiErr.Write(w)
				return
			}
		}

		resp, dispatchErr := s.proxy.Dispatch(r.Context(), input)
		if dispatchErr != nil {
			httpx.FromError(dispatchErr).Write(w)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		copyBody(w, resp)
	}
}

// authenticate resolves the caller against the static proxy key set and
// enforces optional HMAC request signing. The body arrives already read and
// size-bounded by the handler, so no request reads its own stream twice.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, body []byte) (*auth.Identity, *httpx.Error) {
	raw := auth.ExtractBearer(r.Header.Get("Authorization"))
	if raw == "" {
		return nil, httpx.Unauthorized("missing or invalid Authorization header")
	}

	cfg := s.proxy.Config()

	// Each request builds the hash set; the key list is small and reload
	// replaces cfg wholesale, so there is nothing to invalidate.
	hash := auth.HashKey(raw)
	known := false
	for _, k := range cfg.ProxyKeys {
		if auth.HashKey(k) == hash {
			known = true
			break
		}
	}
	if !known {
		return nil, httpx.Unauthorized("invalid API key")
	}

	identity := &auth.Identity{
		Role:      auth.RoleUser,
		KeyHash:   hash,
		KeyPrefix: auth.PrefixFromKey(raw),
	}

	if cfg.GeneralSettings.HMACEnabled {
		signature := r.Header.Get("X-Signature")
		if signature == "" {
			return nil, httpx.Unauthorized("missing X-Signature header")
		}
		if !auth.VerifyHMAC(cfg.GeneralSettings.HMACSecret, body, signature) {
			return nil, httpx.Unauthorized("invalid HMAC signature")
		}
	}

	return identity, nil
}

// checkDims validates an embeddings request's "dimensions" against the
// model's configured ceiling. The parameter still passes upstream untouched.
func (s *Server) checkDims(body []byte, canonical string) *httpx.Error {
	req := peekInt(body, "dimensions", 0)
	if req == 0 {
		return nil
	}
	var dims int
	for _, entry := range s.proxy.Config().Slots(canonical) {
		if entry.Dims == nil {
			return nil
		}
		dims = *entry.Dims
		break
	}
	if dims == 0 || req == dims {
		return nil
	}
	return httpx.BadRequest(
		fmt.Sprintf("model '%s' supports a fixed output size of %d dimensions, however %d was requested", canonical, dims, req))
}

// listModels serves GET /v1/models.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(w, r, nil); err != nil {
		err.Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.ModelListResponse(s.proxy.Router().ModelNames()))
}

// getModel serves GET /v1/models/{id}.
func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(w, r, nil); err != nil {
		err.Write(w)
		return
	}
	id := r.PathValue("id")
	cfg := s.proxy.Config()
	canonical := cfg.Canonical(id)
	if len(cfg.Slots(canonical)) == 0 {
		httpx.NotFound(fmt.Sprintf("model '%s' not found", id)).Write(w)
		return
	}
	listing := httpx.ModelListResponse([]string{canonical})
	httpx.WriteJSON(w, http.StatusOK, listing["data"].([]map[string]any)[0])
}

// Version is reported by /health.
const Version = "2.1.0"

// Shutdown drains in-flight requests and waits for them to finish.
func (s *Server) Shutdown(ctx context.Context) bool {
	s.proxy.BeginDrain(s.opts.DrainTimeout)
	if s.limiter != nil {
		s.limiter.Stop()
	}
	return true
}

// readBody reads and closes a request body, bounded by the MaxBytesReader the
// caller installed.
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// copyBody streams a proxied response to the client. For a streaming response
// this flushes after every chunk so tokens reach the client as they arrive
// rather than sitting in a buffer.
func copyBody(w http.ResponseWriter, resp *http.Response) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func peekString(body []byte, field string) string {
	m, ok := decodeMap(body)
	if !ok {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

func peekInt(body []byte, field string, def int) int {
	m, ok := decodeMap(body)
	if !ok {
		return def
	}
	switch v := m[field].(type) {
	case float64:
		if v <= 0 {
			return def
		}
		return int(v)
	case int:
		return v
	}
	return def
}

func decodeMap(body []byte) (map[string]any, bool) {
	var m map[string]any
	if err := jsonUnmarshal(body, &m); err != nil {
		return nil, false
	}
	return m, true
}
