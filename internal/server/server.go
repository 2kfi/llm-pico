// Package server exposes the proxy over HTTP: the OpenAI-compatible endpoints,
// the admin API, and the dashboard assets.
package server

import (
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
	"github.com/arkan/llm-pico/internal/store"
)

// Options configures a Server.
type Options struct {
	DB     *store.Store
	Proxy  *proxy.Service
	Logger *slog.Logger
	// MaxBodyBytes bounds request bodies. Audio uploads are the largest
	// legitimate payload, so the default leaves generous headroom.
	MaxBodyBytes int64
	// TrustProxyHeaders honours X-Forwarded-For for IP allowlists. It must
	// only be enabled behind a proxy that overwrites the header.
	TrustProxyHeaders bool
	// DrainTimeout bounds how long shutdown waits for in-flight requests.
	DrainTimeout time.Duration
}

// Server owns the HTTP surface.
type Server struct {
	db       *store.Store
	proxy    *proxy.Service
	log      *slog.Logger
	verifier *auth.Verifier
	limiter  *ratelimit.Limiter
	admin    *adminAPI
	mux      *http.ServeMux
	opts     Options
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
		db:       opts.DB,
		proxy:    opts.Proxy,
		log:      opts.Logger,
		verifier: auth.NewVerifier(opts.DB),
		limiter:  opts.Proxy.Limiter(),
		opts:     opts,
	}
	s.admin = &adminAPI{server: s}
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

	s.admin.register(mux)

	mux.HandleFunc("/", s.static)
	return mux
}

// handle builds a proxy handler for one route type.
func (s *Server) handle(route proxy.RouteType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, authErr := s.authenticate(w, r)
		if authErr != nil {
			authErr.Write(w)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)
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

		input := proxy.DispatchInput{
			Body:      body,
			Identity:  identity,
			RouteType: route,
			MasterKey: s.proxy.Config().GeneralSettings.MasterKey,
		}

		switch route {
		case proxy.RouteSTT:
			audio, fields, apiErr := s.parseAudio(r)
			if apiErr != nil {
				apiErr.Write(w)
				return
			}
			input.Audio = audio
			input.Model = fields["model"]
			input.MaxTokens = 0
		case proxy.RouteTTS:
			if !json.Valid(body) {
				httpx.BadRequest("invalid JSON body").Write(w)
				return
			}
			input.Model = peekString(body, "model")
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

		if !identity.CanAccessModel(input.Model) {
			httpx.Forbidden(fmt.Sprintf("model '%s' not allowed for this key", input.Model)).Write(w)
			return
		}

		if !s.proxy.Router().Has(input.Model) {
			httpx.NotFound(fmt.Sprintf("model '%s' not available", input.Model)).Write(w)
			return
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

// authenticate resolves the caller, enforcing the IP allowlist and optional
// HMAC request signing.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*auth.Identity, *httpx.Error) {
	raw := auth.ExtractBearer(r.Header.Get("Authorization"))
	if raw == "" {
		return nil, httpx.Unauthorized("missing or invalid Authorization header")
	}

	cfg := s.proxy.Config()
	identity, err := s.verifier.Verify(r.Context(), raw, cfg.GeneralSettings.MasterKey)
	if err != nil {
		return nil, httpx.Unauthorized("invalid API key")
	}

	// The master key is not subject to key-level restrictions.
	if identity.Role == auth.RoleUser {
		clientIP := auth.ClientIP(r, s.opts.TrustProxyHeaders)
		if !auth.CheckIP(clientIP, identity.IPAllowlist) {
			return nil, httpx.Forbidden("IP not allowed")
		}
	}

	if cfg.GeneralSettings.HMACEnabled {
		signature := r.Header.Get("X-Signature")
		if signature == "" {
			return nil, httpx.Unauthorized("missing X-Signature header")
		}
		body, err := readBody(r)
		if err != nil {
			return nil, httpx.BadRequest("could not read request body")
		}
		if !auth.VerifyHMAC(cfg.GeneralSettings.HMACSecret, body, signature) {
			return nil, httpx.Unauthorized("invalid HMAC signature")
		}
		// The body has been consumed; make it available to the handler.
		r.Body = newBodyReader(body)
	}

	return identity, nil
}

// listModels serves GET /v1/models.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	identity, err := s.authenticate(w, r)
	if err != nil {
		err.Write(w)
		return
	}
	names := s.proxy.Router().ModelNames()
	if identity.Role == auth.RoleUser && identity.Allowlist != nil {
		allowed := map[string]bool{}
		for _, m := range identity.Allowlist {
			allowed[m] = true
		}
		filtered := make([]string, 0, len(names))
		for _, n := range names {
			if allowed[n] {
				filtered = append(filtered, n)
			}
		}
		names = filtered
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.ModelListResponse(names))
}

// getModel serves GET /v1/models/{id}.
func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	identity, err := s.authenticate(w, r)
	if err != nil {
		err.Write(w)
		return
	}
	id := r.PathValue("id")
	known := false
	for _, n := range s.proxy.Router().ModelNames() {
		if n == id {
			known = true
			break
		}
	}
	if !known {
		httpx.NotFound(fmt.Sprintf("model '%s' not found", id)).Write(w)
		return
	}
	if identity.Role == auth.RoleUser && !identity.CanAccessModel(id) {
		httpx.Forbidden(fmt.Sprintf("model '%s' not allowed", id)).Write(w)
		return
	}
	listing := httpx.ModelListResponse([]string{id})
	httpx.WriteJSON(w, http.StatusOK, listing["data"].([]map[string]any)[0])
}

// Version is reported by /health.
const Version = "2.0.0"

// Shutdown drains in-flight requests and waits for them to finish.
func (s *Server) Shutdown(ctx context.Context) bool {
	s.proxy.BeginDrain(s.opts.DrainTimeout)
	if s.limiter != nil {
		s.limiter.Stop(ctx)
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
