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
	"sync/atomic"
	"time"

	"github.com/arkan/llm-pico/internal/auth"
	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/proxy"
	"github.com/arkan/llm-pico/internal/ratelimit"
	"github.com/arkan/llm-pico/internal/version"
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

	// keySet caches the hashed proxy keys of one configuration snapshot, so
	// authenticating a request is a map lookup rather than a full scan.
	keySet atomic.Pointer[proxyKeySet]
}

// proxyKeySet is the hashed key set of one configuration snapshot. The config
// pointer is the cache key: a reload stores a new pointer and the set is
// rebuilt on the next request.
type proxyKeySet struct {
	cfg  *config.Config
	keys map[string]struct{}
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
	// The legacy text-completions shape was never wired through; reject it
	// with a clear message so old tooling does not guess at it.
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		httpx.BadRequest("legacy completions endpoint is not supported; use /v1/chat/completions").Write(w)
	})
	mux.HandleFunc("POST /v1/embeddings", s.handle(proxy.RouteEmbeddings))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handle(proxy.RouteSTT))
	mux.HandleFunc("POST /v1/audio/speech", s.handle(proxy.RouteTTS))
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("GET /v1/models/{id}", s.getModel)
	mux.HandleFunc("GET /v1/status", s.handleStatus)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.String()})
	})

	return mux
}

// handle builds a proxy handler for one route type.
func (s *Server) handle(route proxy.RouteType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Audio uploads are the largest legitimate payload, so they get
		// their own ceiling; every other route shares the general bound.
		// The limit is installed before anything reads the body.
		limit := s.opts.MaxBodyBytes
		if route == proxy.RouteSTT {
			limit = maxAudioBytes
		}

		// The bearer check needs no body, so it runs before the read: an
		// unauthenticated caller never forces the server to buffer
		// anything.
		identity, authErr := s.resolveIdentity(r)
		if authErr != nil {
			authErr.Write(w)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, limit)
		body, err := readBody(r)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				// 413, not 400: an agent must read a size rejection as
				// "retry with less context", not as a schema error.
				(&httpx.Error{
					Status:  http.StatusRequestEntityTooLarge,
					Type:    httpx.TypeBadRequest,
					Message: "request body too large",
				}).Write(w)
				return
			}
			httpx.BadRequest("could not read request body").Write(w)
			return
		}

		if authErr := s.verifyHMAC(r, body); authErr != nil {
			authErr.Write(w)
			return
		}

		input := proxy.DispatchInput{
			Body:      body,
			Identity:  identity,
			RouteType: route,
		}

		// One decode serves every peek below. The old path validated the body
		// and then unmarshalled it again for each field, which is real memory
		// on a large agent payload.
		var fields map[string]any
		if route != proxy.RouteSTT {
			decoded, ok := decodeMap(body)
			if !ok {
				httpx.BadRequest("invalid JSON body").Write(w)
				return
			}
			fields = decoded
		}

		switch route {
		case proxy.RouteSTT:
			// The multipart parser needs a live reader; the body has
			// already been bounded and read above.
			r.Body = io.NopCloser(bytes.NewReader(body))
			audio, formFields, apiErr := s.parseAudio(r)
			if apiErr != nil {
				apiErr.Write(w)
				return
			}
			input.Audio = audio
			input.Model = formFields["model"]
			// ~32 kB of compressed audio per second is the usual bitrate
			// band; the estimate only feeds the audio-seconds limits.
			input.AudioSeconds = max(1, len(audio.Data)/32000)
			input.MaxTokens = 0
		case proxy.RouteTTS:
			input.Model = stringField(fields, "model")
			input.Chars = len(stringField(fields, "input"))
			// Spoken cadence is roughly fifteen characters per second.
			input.AudioSeconds = (input.Chars + 14) / 15
			input.MaxTokens = 0
		default:
			input.Model = stringField(fields, "model")
			// A request that does not set max_tokens gets the provider's own
			// default; reserving 4096 tokens against the rate limiter would
			// over-charge callers for budget they did not ask for.
			input.MaxTokens = requestedMaxTokens(fields)
		}

		if input.Model == "" {
			httpx.BadRequest("field 'model' is required").Write(w)
			return
		}

		cfg := s.proxy.Config()
		if len(cfg.Slots(input.Model)) == 0 {
			httpx.NotFound(fmt.Sprintf("model '%s' not available", input.Model)).Write(w)
			return
		}

		if route == proxy.RouteEmbeddings {
			if apiErr := s.checkDims(fields, input.Model); apiErr != nil {
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

		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		copyBody(w, resp)
	}
}

// hopHeaders are response headers the proxy must not forward. Adapters rewrite
// the upstream body, so the upstream's framing headers describe bytes the
// caller never receives; forwarding Content-Length in particular makes clients
// abort after the converted body comes up short.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Content-Length":      true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyResponseHeaders copies upstream headers onto the caller's response,
// dropping hop-by-hop and body-framing fields.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// resolveIdentity resolves the caller against the static proxy key set. It
// needs only headers, so the handler can run it before reading the body and
// reject an unauthenticated caller without buffering anything.
func (s *Server) resolveIdentity(r *http.Request) (*auth.Identity, *httpx.Error) {
	raw := auth.ExtractBearer(r.Header.Get("Authorization"))
	if raw == "" {
		return nil, httpx.Unauthorized("missing or invalid Authorization header")
	}

	cfg := s.proxy.Config()

	// Each request builds the hash set; the key list is small and reload
	// replaces cfg wholesale, so there is nothing to invalidate.
	hash := auth.HashKey(raw)
	if _, known := s.proxyKeySet(cfg)[hash]; !known {
		return nil, httpx.Unauthorized("invalid API key")
	}

	identity := &auth.Identity{
		Role:      auth.RoleUser,
		KeyHash:   hash,
		KeyPrefix: auth.PrefixFromKey(raw),
	}
	return identity, nil
}

// proxyKeySet returns the hashed key set for cfg, rebuilding it when the
// configuration snapshot has been swapped by a reload.
func (s *Server) proxyKeySet(cfg *config.Config) map[string]struct{} {
	if cached := s.keySet.Load(); cached != nil && cached.cfg == cfg {
		return cached.keys
	}
	keys := make(map[string]struct{}, len(cfg.ProxyKeys))
	for _, k := range cfg.ProxyKeys {
		keys[auth.HashKey(k)] = struct{}{}
	}
	s.keySet.Store(&proxyKeySet{cfg: cfg, keys: keys})
	return keys
}

// verifyHMAC enforces optional HMAC request signing over the caller's body,
// which the handler has already read and size-bounded.
func (s *Server) verifyHMAC(r *http.Request, body []byte) *httpx.Error {
	cfg := s.proxy.Config()
	if !cfg.GeneralSettings.HMACEnabled {
		return nil
	}
	signature := r.Header.Get("X-Signature")
	if signature == "" {
		return httpx.Unauthorized("missing X-Signature header")
	}
	if !auth.VerifyHMAC(cfg.GeneralSettings.HMACSecret, body, signature) {
		return httpx.Unauthorized("invalid HMAC signature")
	}
	return nil
}

// checkDims validates an embeddings request's "dimensions" against the
// model's configured ceiling. The parameter still passes upstream untouched.
func (s *Server) checkDims(fields map[string]any, canonical string) *httpx.Error {
	req := intField(fields, "dimensions", 0)
	if req == 0 {
		return nil
	}
	dims := 0
	for _, entry := range s.proxy.Config().Slots(canonical) {
		// Only slots that declare dims constrain the request: an
		// unconstrained slot must not veto, but a later slot may.
		if entry.Dims == nil {
			continue
		}
		dims = *entry.Dims
		if req == dims {
			return nil
		}
	}
	if dims == 0 {
		return nil
	}
	return httpx.BadRequest(
		fmt.Sprintf("model '%s' supports a fixed output size of %d dimensions, however %d was requested", canonical, dims, req))
}

// listModels serves GET /v1/models.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if _, err := s.resolveIdentity(r); err != nil {
		err.Write(w)
		return
	}
	models := s.modelInfos(s.proxy.Router().ModelNames())
	httpx.WriteJSON(w, http.StatusOK, httpx.ModelListResponse(models))
}

// modelInfos aggregates the per-slot capability flags for each listed model.
//
// Media capabilities are unioned across slots: a model that accepts images on
// any slot advertises it, while dispatch still gates every slot on its own
// flags. Agent-readiness is an intersection instead: advertising tool support
// for a model whose next slot cannot run a tool call would be a lie.
func (s *Server) modelInfos(names []string) []httpx.ModelInfo {
	cfg := s.proxy.Config()
	infos := make([]httpx.ModelInfo, 0, len(names))
	for _, name := range names {
		slots := cfg.Slots(name)
		info := httpx.ModelInfo{ID: name, Tools: len(slots) > 0, Reasoning: len(slots) > 0}
		for _, entry := range slots {
			info.Images = info.Images || entry.Images
			info.Audio = info.Audio || entry.Audio
			info.Video = info.Video || entry.Video
			info.Thinking = info.Thinking || entry.Thinking
			info.Tools = info.Tools && entry.Tools
			info.Reasoning = info.Reasoning && entry.Reasoning
		}
		// A live probe that reports no tool support is the last word.
		if caps, ok := s.proxy.Capability(name); ok && !caps.SupportsTools {
			info.Tools = false
		}
		infos = append(infos, info)
	}
	return infos
}

// getModel serves GET /v1/models/{id}.
func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	if _, err := s.resolveIdentity(r); err != nil {
		err.Write(w)
		return
	}
	id := r.PathValue("id")
	cfg := s.proxy.Config()
	if len(cfg.Slots(id)) == 0 {
		httpx.NotFound(fmt.Sprintf("model '%s' not found", id)).Write(w)
		return
	}
	listing := httpx.ModelListResponse(s.modelInfos([]string{id}))
	if data, ok := listing["data"].([]map[string]any); ok && len(data) > 0 {
		httpx.WriteJSON(w, http.StatusOK, data[0])
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listing)
}

// handleStatus serves GET /v1/status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := s.resolveIdentity(r); err != nil {
		err.Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.proxy.Router().AllStatus())
}

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

// stringField reads a string field from an already-decoded body.
func stringField(fields map[string]any, name string) string {
	s, _ := fields[name].(string)
	return s
}

// requestedMaxTokens reads the completion budget, preferring OpenAI's newer
// max_completion_tokens so reservation and context checks match what the
// adapters actually send upstream.
func requestedMaxTokens(fields map[string]any) int {
	if n := intField(fields, "max_completion_tokens", 0); n > 0 {
		return n
	}
	return intField(fields, "max_tokens", 0)
}

// intField reads a positive integer field from an already-decoded body.
func intField(fields map[string]any, name string, def int) int {
	switch v := fields[name].(type) {
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

// decodeMap decodes a request body once, so every later peek reads the same
// map instead of re-parsing the payload.
func decodeMap(body []byte) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	return m, true
}
