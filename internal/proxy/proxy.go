// Package proxy dispatches a request to an upstream provider, retrying across
// credentials, models and fallback chains, and accounting for what it spent.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arkan/llm-pico/internal/auth"
	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/observability"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/ratelimit"
	"github.com/arkan/llm-pico/internal/router"
	"github.com/arkan/llm-pico/internal/usage"
)

// RouteType selects which upstream operation a request performs.
type RouteType string

const (
	RouteChat       RouteType = "chat"
	RouteEmbeddings RouteType = "embeddings"
	RouteSTT        RouteType = "stt"
	RouteTTS        RouteType = "tts"
)

// DegradationMode controls how the proxy behaves under stress.
type DegradationMode string

const (
	// DegradationNormal processes every request.
	DegradationNormal DegradationMode = "normal"
	// DegradationReject refuses requests immediately with 503.
	DegradationReject DegradationMode = "reject"
	// DegradationFallbackOnly serves only models explicitly marked as
	// fallbacks, shedding load from expensive primaries.
	DegradationFallbackOnly DegradationMode = "fallback_only"
)

// Service is the request-handling core. One instance serves the process.
//
// Configuration, the routing table and the provider registry are held behind
// atomic pointers so a hot reload swaps them without blocking in-flight
// requests and without any request observing a half-applied configuration.
type Service struct {
	limiter *ratelimit.Limiter
	Latency *observability.LatencyTracker
	Log     *slog.Logger

	config    atomic.Pointer[config.Config]
	routes    atomic.Pointer[router.Router]
	providers atomic.Pointer[providers.Registry]
	mode      atomic.Value // DegradationMode

	draining atomic.Bool
	// inFlight covers dispatches; streamWG covers the copy goroutines that
	// finish streaming a response after Dispatch has returned. Drain waits
	// for both so graceful shutdown does not cut a stream mid-flight.
	inFlight sync.WaitGroup
	streamWG sync.WaitGroup

	// queue bounds concurrent requests in fallback_only mode.
	sem chan struct{}
}

// NewService builds a Service from a configuration snapshot.
func NewService(cfg *config.Config, providers *providers.Registry, logger *slog.Logger) *Service {
	s := &Service{
		Latency: observability.NewLatencyTracker(1000),
		Log:     logger,
		sem:     make(chan struct{}, 512),
	}
	s.config.Store(cfg)
	s.routes.Store(router.New(cfg))
	s.providers.Store(providers)
	s.mode.Store(DegradationNormal)
	return s
}

// SetLimiter attaches the rate limiter built from the loaded configuration.
func (s *Service) SetLimiter(l *ratelimit.Limiter) { s.limiter = l }

// Reload swaps the configuration and routing table without touching in-flight
// requests; a new request sees either the old or the new snapshot in full.
func (s *Service) Reload(cfg *config.Config, reg *providers.Registry) {
	if reg != nil {
		s.providers.Store(reg)
	}
	s.config.Store(cfg)
	s.routes.Store(router.New(cfg))
}

// Config returns the current configuration snapshot.
func (s *Service) Config() *config.Config { return s.config.Load() }

// Registry returns the current provider registry snapshot.
func (s *Service) Registry() *providers.Registry { return s.providers.Load() }

// Router returns the current routing table.
func (s *Service) Router() *router.Router { return s.routes.Load() }

// Mode returns the current degradation mode.
func (s *Service) Mode() DegradationMode {
	m, _ := s.mode.Load().(DegradationMode)
	return m
}

// SetMode changes the degradation mode.
func (s *Service) SetMode(m DegradationMode) { s.mode.Store(m) }

// BeginDrain stops admitting new requests and waits for in-flight ones to
// finish, up to a deadline.
func (s *Service) BeginDrain(timeout time.Duration) bool {
	s.draining.Store(true)
	done := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		s.streamWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Draining reports whether the instance is shutting down.
func (s *Service) Draining() bool { return s.draining.Load() }

// DispatchInput describes one incoming request.
type DispatchInput struct {
	Body      []byte
	Model     string
	MaxTokens int
	Identity  *auth.Identity
	RouteType RouteType
	Audio     *providers.AudioFile
	// Chars and AudioSeconds pre-carries the request-sized estimates the
	// character and audio windows charge; the server computes these from the
	// request payload before dispatch.
	Chars        int
	AudioSeconds int
}

// quotaError wraps a rejection by this proxy's own limiter, which unlike an
// upstream 429 is terminal: switching slots cannot create quota.
type quotaError struct {
	err *httpx.Error
}

func (e *quotaError) Error() string { return e.err.Error() }

func (e *quotaError) Unwrap() error { return e.err }

// Dispatch resolves a model, enforces limits, calls upstream, and accounts.
//
// Slots are walked in declared priority order. A slot's own quota exhaustion
// or upstream failure moves to the next slot; a caller-side rejection is
// terminal.
func (s *Service) Dispatch(ctx context.Context, in DispatchInput) (*http.Response, error) {
	if s.Draining() {
		return nil, httpx.Draining()
	}

	s.inFlight.Add(1)
	defer s.inFlight.Done()

	cfg := s.Config()
	r := s.Router()

	tracer := observability.NewTracer(newRequestID())

	admittedFallback, err := s.admit(ctx, in, cfg)
	if err != nil {
		return nil, err
	}

	model := cfg.Canonical(in.Model)
	allSlots := kindSlots(cfg, model, in.RouteType)
	if len(allSlots) == 0 {
		return nil, httpx.BadRequest(
			fmt.Sprintf("model '%s' does not support this endpoint", in.Model))
	}

	slots := slotsWithinContext(allSlots, in)
	if len(slots) == 0 {
		return nil, contextTooLongError(allSlots, in)
	}

	ac := attemptCtx{tracer: tracer, cfg: cfg, router: r, semOwned: admittedFallback}

	var (
		lastErr error
		tried   []string
	)
	for i, slot := range slots {
		tried = append(tried, slot.Provider)
		resp, err := s.attemptSlot(ctx, ac, in, model, slot)
		if err == nil {
			if i > 0 && resp != nil {
				httpx.ApplyHeader(resp.Header, httpx.ChainHeaders(slot.Provider, tried, i))
			}
			return resp, nil
		}
		lastErr = err

		// A slot's own quota is its latitude: move to the next slot, which
		// may still have room. Caller-fault and terminal errors stop the walk.
		var qe *quotaError
		if errors.As(err, &qe) {
			continue
		}
		if isTerminal(err) || isClientRejection(err) {
			break
		}
		if i == len(slots)-1 {
			r.RecordModelFailure(model)
		}
	}

	if lastErr == nil {
		lastErr = httpx.Upstream(http.StatusBadGateway, "all upstream providers failed")
	}
	return nil, httpx.FromError(lastErr)
}

// kindSlots returns the model's slots that serve the route's operation kind,
// in configuration order.
func kindSlots(cfg *config.Config, modelName string, rt RouteType) []config.ModelEntry {
	var out []config.ModelEntry
	for _, entry := range cfg.Slots(modelName) {
		if kindOf(rt) == entry.Kind {
			out = append(out, entry)
		}
	}
	return out
}

func kindOf(rt RouteType) config.Kind {
	switch rt {
	case RouteEmbeddings:
		return config.KindEmbeddings
	case RouteSTT:
		return config.KindSTT
	case RouteTTS:
		return config.KindTTS
	default:
		return config.KindChat
	}
}

// slotsWithinContext drops slots whose context budget cannot hold the
// estimated request. It returns the surviving slots; an empty result means no
// slot can serve the request within its context window.
func slotsWithinContext(slots []config.ModelEntry, in DispatchInput) []config.ModelEntry {
	var out []config.ModelEntry
	for _, entry := range slots {
		ctx := entry.EffectiveCtx()
		if ctx == nil {
			out = append(out, entry)
			continue
		}
		if estimateTokens(in) > int64(*ctx) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// contextTooLongError renders the OpenAI-style rejection for a request that
// no slot could accommodate.
func contextTooLongError(slots []config.ModelEntry, in DispatchInput) error {
	maxCtx := 0
	for _, entry := range slots {
		if ctx := entry.EffectiveCtx(); ctx != nil && *ctx > maxCtx {
			maxCtx = *ctx
		}
	}
	requested := estimateTokens(in)
	return &httpx.Error{
		Status: http.StatusBadRequest,
		Type:   httpx.TypeBadRequest,
		Message: fmt.Sprintf(
			"This model's maximum context length is %d tokens, however you requested %d tokens. "+
				"Please reduce the length of the messages or the max_tokens value.",
			maxCtx, requested),
	}
}

// estimateTokens is the preflight body-size approximation used for context and
// token-window accounting: four bytes per token plus completion budget.
func estimateTokens(in DispatchInput) int64 {
	total := int64(len(in.Body)/4) + int64(in.MaxTokens)
	if total <= 0 {
		return 1
	}
	return total
}

// attemptCtx is the immutable context of one dispatched request.
type attemptCtx struct {
	tracer *observability.Tracer
	cfg    *config.Config
	router *router.Router
	// semOwned records that this request holds a fallback_only semaphore
	// slot, which release must hand back.
	semOwned bool
}

// admit applies the current degradation mode before any work is done. It
// reports whether the request took a fallback_only semaphore slot, so release
// can hand back exactly what this request took even if the mode changes
// mid-flight.
func (s *Service) admit(ctx context.Context, in DispatchInput, cfg *config.Config) (semOwned bool, err error) {
	switch s.Mode() {
	case DegradationReject:
		return false, httpx.Degraded("service degraded: requests temporarily rejected")
	case DegradationFallbackOnly:
		// Only admit a bounded number of concurrent requests, so a provider
		// that is failing can be given room to recover.
		select {
		case s.sem <- struct{}{}:
			return true, nil
		case <-ctx.Done():
			return true, httpx.UpstreamTimeout("request cancelled while queued")
		default:
			return false, httpx.Degraded("service degraded: concurrency limit reached")
		}
	default:
		return false, nil
	}
}

// release frees a semaphore slot taken by admit.
func (s *Service) release(semOwned bool) {
	if !semOwned {
		return
	}
	select {
	case <-s.sem:
	default:
	}
}

// reservation is the per-request rate-limit accounting, threaded through the
// attempt so a retry loop cannot double-reserve.
type reservation struct {
	slot       string
	model      string
	hash       string
	limits     ratelimit.Limits
	costs      ratelimit.Costs
	tokens     int64
	headers    http.Header
	reconciled bool
}

// keyHash returns the attribution hash for the caller.
func keyHash(in DispatchInput) string {
	if in.Identity == nil {
		return ""
	}
	return in.Identity.KeyHash
}

// attemptSlot makes the configured number of tries against one provider slot
// of the model.
func (s *Service) attemptSlot(ctx context.Context, ac attemptCtx, in DispatchInput, modelName string, slot config.ModelEntry) (*http.Response, error) {
	maxTries := max(1, ac.cfg.RouterSettings.NumRetries+1)
	var (
		res       *reservation
		lastErr   error
		tracedOne bool
	)

	for try := 0; try < maxTries; try++ {
		target, err := ac.router.ResolveSlot(modelName, slot.Provider)
		if err != nil {
			var cooldown *router.CooldownError
			if errors.As(err, &cooldown) {
				// A router cooldown is the slot's own quota latitude, not a
				// caller fault: wrap it as a quota error so Dispatch keeps
				// walking the remaining fallback slots.
				return nil, &quotaError{err: httpx.RateLimited(
					fmt.Sprintf("all keys for provider '%s' are rate-limited", slot.Provider),
					int(cooldown.RetryAfter.Seconds())+1,
				)}
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, httpx.NotFound(fmt.Sprintf("model '%s' not available", modelName))
		}

		if !tracedOne {
			ac.tracer.Add("resolve:"+modelName, modelName, target.ProviderSlug, "ok", "", time.Time{})
			tracedOne = true
		}

		// Chat requests carrying image parts need a vision-capable entry;
		// kind filtering has already handled every other route mismatch.
		if in.RouteType == RouteChat && providers.HasImageInput(in.Body) && !target.Model.Images {
			return nil, httpx.BadRequest(fmt.Sprintf("model '%s' does not support image inputs", in.Model))
		}

		// Limits are charged per slot, plus once at model level; a retry loop
		// cannot re-reserve what is already held.
		if res == nil {
			quota, err := s.reserve(in, modelName, slot)
			if quota == nil {
				return nil, &quotaError{err: err}
			}
			res = quota
		}

		adapter := s.Registry().AdapterFor(target.ProviderSlug, target.APIBase)
		resp, _, err := s.call(ctx, ac, in, target, adapter, res)

		if err == nil {
			ac.router.RecordSuccess(target)
			ac.tracer.Add("proxy:"+modelName, modelName, target.ProviderSlug, "ok", "", time.Time{})
			if resp != nil {
				httpx.ApplyHeader(resp.Header, res.headers)
				resp.Header.Set("X-Request-Id", ac.tracer.ID())
			}
			// Buffer calls have already settled inline; stream calls
			// settle from their copy goroutine once usage is known, and
			// their finalize is a no-op by design.
			return resp, nil
		}

		var apiErr *httpx.Error
		if errors.As(err, &apiErr) && isTerminal(err) {
			ac.router.RecordFailure(target, apiErr.Status)
			ac.tracer.Add("error:"+modelName, modelName, target.ProviderSlug, "fail",
				fmt.Sprint(apiErr.Status), time.Time{})
			s.refund(res)
			return nil, err
		}

		status := statusOf(err)
		ac.router.RecordFailure(target, status)
		ac.tracer.Add("error:"+modelName, modelName, target.ProviderSlug, "fail",
			fmt.Sprint(status), time.Time{})
		lastErr = err
	}

	// Every attempt failed: release the reservation so the caller is not
	// charged for work that never happened.
	s.refund(res)
	return nil, lastErr
}

// reserve consumes capacity from the slot's and the model's limit sets. On
// exhaustion it returns a nil reservation plus an error carrying the
// X-RateLimit headers of the window that rejected.
func (s *Service) reserve(in DispatchInput, modelName string, slot config.ModelEntry) (*reservation, *httpx.Error) {
	if s.limiter == nil {
		return &reservation{}, nil
	}

	costs := ratelimit.Costs{
		Tokens:   estimateTokens(in),
		Chars:    int64(in.Chars),
		AudioSec: int64(in.AudioSeconds),
	}
	limits := ratelimit.Limits{
		ratelimit.RPM:  intPtr(slot.RPM),
		ratelimit.RPD:  intPtr(slot.RPD),
		ratelimit.TPM:  intPtr(slot.TPM),
		ratelimit.TPD:  intPtr(slot.TPD),
		ratelimit.CPM:  intPtr(slot.CPM),
		ratelimit.CPD:  intPtr(slot.CPD),
		ratelimit.ASEC: intPtr(slot.AudioSecPerDay),
		ratelimit.AMIN: intPtr(slot.AudioMin),
	}

	hash := keyHash(in)
	m := ratelimit.LevelModel

	// LevelModel cells are keyed by slot and model, so each provider slot is
	// charged the model's limits independently: one slot running dry must not
	// drain the quota of another.
	if rejection := s.limiter.CheckAndReserve(hash, modelName, slot.Provider, m, limits, costs); rejection != nil {
		return nil, s.quotaRejection(rejection, hash, modelName, slot.Provider, limits)
	}

	headers := httpx.RateLimitHeaders(s.limiter, hash, modelName, slot.Provider, m, limits)
	return &reservation{
		slot:    slot.Provider,
		model:   modelName,
		hash:    hash,
		limits:  limits,
		costs:   costs,
		tokens:  costs.Tokens,
		headers: headers,
	}, nil
}

// quotaRejection renders a limiter rejection as a headered 429.
func (s *Service) quotaRejection(r *ratelimit.Rejection, keyHash, modelName, slot string, limits ratelimit.Limits) *httpx.Error {
	e := httpx.RateLimited(
		fmt.Sprintf("rate limit exceeded: %s", r.Window),
		int(r.RetryAfter.Seconds())+1,
	)
	e.Headers = httpx.RateLimitHeaders(s.limiter, keyHash, modelName, slot, ratelimit.LevelModel, limits)
	return e
}

// settle reconciles the token estimate against what the provider reported. It
// runs at most once, since a second call would refund capacity never taken.
func (s *Service) settle(res *reservation, actual int64) {
	if s.limiter == nil || res == nil || res.model == "" || res.reconciled {
		return
	}
	res.reconciled = true
	s.limiter.Reconcile(res.hash, res.model, res.slot, ratelimit.LevelModel, actual, res.tokens)
}

// refund releases a reservation that never reached a successful upstream call.
func (s *Service) refund(res *reservation) {
	if res == nil || res.model == "" {
		return
	}
	s.limiter.Refund(res.hash, res.model, res.slot, ratelimit.LevelModel, res.limits, res.costs)
}

// callUsage is what a completed upstream call cost.
type callUsage struct {
	TotalTokens int
	Latency     time.Duration
}

// finalizeFn settles accounting for one call exactly once. For a buffered
// response it runs inline; for a stream the copy goroutine runs it when the
// body is drained.
func finalizeFn(s *Service, res *reservation, u *callUsage, err error) {
	total := u.TotalTokens
	if total == 0 && res != nil {
		// A provider reporting no usage is assumed to have consumed
		// exactly what was reserved, which keeps the limiter from drifting
		// in either direction.
		total = int(res.tokens)
	}

	if err == nil {
		s.settle(res, int64(total))
	} else {
		s.refund(res)
	}
}

// call performs the upstream request; accounting settles inline for buffered
// calls and from the stream goroutine for streamed calls.
func (s *Service) call(
	ctx context.Context,
	ac attemptCtx,
	in DispatchInput,
	target *router.Target,
	adapter providers.Adapter,
	res *reservation,
) (*http.Response, func(error), error) {
	req := providers.Request{
		Body:    in.Body,
		Model:   target.Model.ModelParams.Model,
		APIKey:  target.APIKey,
		BaseURL: target.APIBase,
	}

	start := time.Now()
	s.release(ac.semOwned)

	var (
		resp *providers.Response
		u    *callUsage
		err  error
	)

	switch in.RouteType {
	case RouteEmbeddings:
		resp, err = adapter.Embeddings(ctx, req)
		u = &callUsage{}
	case RouteSTT:
		if in.Audio == nil {
			return nil, nil, httpx.BadRequest("field 'file' is required")
		}
		resp, err = adapter.Transcribe(ctx, req, *in.Audio)
		u = &callUsage{TotalTokens: 1}
	case RouteTTS:
		resp, err = adapter.Speak(ctx, req)
		u = &callUsage{TotalTokens: 1}
	default:
		req.Stream = wantsStream(in.Body)
		if req.Stream {
			return s.callStreaming(ctx, ac, in, target, adapter, req, res, start)
		}
		resp, err = adapter.Complete(ctx, req)
		u = &callUsage{}
	}

	if err != nil {
		return nil, nil, upstreamError(err, target.ProviderSlug)
	}

	if in.RouteType == RouteChat {
		u = bufferedUsage(resp.Body)
	}
	u.Latency = time.Since(start)
	s.Latency.Record(target.Model.ModelName, target.ProviderSlug, int(u.Latency.Milliseconds()))

	out := &http.Response{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header.Clone(),
		Body:          io.NopCloser(strings.NewReader(string(resp.Body))),
		ContentLength: int64(len(resp.Body)),
	}
	finalizeFn(s, res, u, nil)
	return out, func(error) {}, nil
}

// callStreaming pipes the upstream stream to the client and settles accounting
// when the body is drained.
func (s *Service) callStreaming(
	ctx context.Context,
	ac attemptCtx,
	in DispatchInput,
	target *router.Target,
	adapter providers.Adapter,
	req providers.Request,
	res *reservation,
	start time.Time,
) (*http.Response, func(error), error) {
	req.Stream = true
	stream, err := adapter.Stream(ctx, req)
	if err != nil {
		return nil, nil, upstreamError(err, target.ProviderSlug)
	}

	pr, pw := io.Pipe()
	info := &callUsage{Latency: time.Since(start)}
	settle := sync.Once{}

	// The caller gets a no-op: for a stream, accounting can only settle
	// from the copy goroutine, once the provider has reported usage.
	// Settling at dispatch time would lock in the pre-reserved estimate.
	streamFinalize := func(err error) {
		settle.Do(func() {
			finalizeFn(s, res, info, err)
		})
	}

	// The stream settles and closes after Dispatch has returned, which
	// BeginDrain's dispatch counter no longer covers; drain therefore
	// tracks this goroutine separately.
	s.streamWG.Add(1)
	go func() {
		defer s.streamWG.Done()
		var last *providers.Usage
		for {
			chunk, err := stream.Next()
			if err != nil {
				if err != io.EOF {
					_ = pw.CloseWithError(err)
					streamFinalize(err)
					return
				}
				break
			}
			if chunk.Usage != nil {
				last = chunk.Usage
			}
			// The sentinel terminates the stream: forward it so clients
			// see their expected closing frame, then stop reading.
			if len(chunk.Data) == 0 {
				if chunk.Done {
					if _, err := pw.Write([]byte("data: [DONE]\n\n")); err != nil {
						break
					}
					break
				}
				continue
			}
			frame := make([]byte, 0, len(chunk.Data)+8)
			frame = append(frame, "data: "...)
			frame = append(frame, chunk.Data...)
			frame = append(frame, '\n', '\n')
			if _, err := pw.Write(frame); err != nil {
				// The client disconnected; stop reading and let the deferred
				// Close release the upstream connection.
				break
			}
		}
		_ = stream.Close()
		_ = pw.Close()

		if last != nil {
			info.TotalTokens = last.TotalTokens
		}
		info.Latency = time.Since(start)
		s.Latency.Record(target.Model.ModelName, target.ProviderSlug, int(info.Latency.Milliseconds()))
		streamFinalize(nil)
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":      {"text/event-stream"},
			"Cache-Control":     {"no-cache"},
			"Connection":        {"keep-alive"},
			"X-Accel-Buffering": {"no"},
		},
		Body: pr,
	}, func(error) {}, nil
}

// bufferedUsage extracts token counts from a non-streaming response body.
func bufferedUsage(body []byte) *callUsage {
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return &callUsage{}
	}
	total := payload.Usage.TotalTokens
	if total == 0 {
		total = payload.Usage.PromptTokens + payload.Usage.CompletionTokens
	}
	return &callUsage{TotalTokens: total}
}

// upstreamError converts a provider failure into an API error, passing the
// upstream status and body through so clients see what the provider said.
func upstreamError(err error, slug string) error {
	var endpoint *providers.EndpointError
	if errors.As(err, &endpoint) {
		msg := string(endpoint.Body)
		if msg == "" {
			msg = fmt.Sprintf("upstream returned %d", endpoint.StatusCode)
		}
		status := endpoint.StatusCode
		if status < 400 {
			status = http.StatusBadGateway
		}
		return &httpx.Error{
			Status:  status,
			Type:    usage.Classify(status, msg),
			Message: truncate(msg, 2000),
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return httpx.UpstreamTimeout(fmt.Sprintf("upstream %s timed out", slug))
	}
	return httpx.Upstream(http.StatusBadGateway, fmt.Sprintf("upstream %s request failed: %v", slug, err))
}

// isTerminal reports whether an error is the caller's to fix, in which case
// retrying another provider would only waste budget.
func isTerminal(err error) bool {
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	// 429 is excluded: it is a rate limit, and another key or model may still
	// have capacity.
	if apiErr.Status == 429 || apiErr.Status == http.StatusTooManyRequests {
		return false
	}
	return apiErr.Status >= 400 && apiErr.Status < 500
}

// isClientRejection reports whether the failure is a limit the caller has
// exhausted rather than a fault in the model or its providers. These must not
// influence model health.
func isClientRejection(err error) bool {
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Type == httpx.TypeRateLimited
}

func statusOf(err error) int {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	var endpoint *providers.EndpointError
	if errors.As(err, &endpoint) {
		return endpoint.StatusCode
	}
	return http.StatusBadGateway
}

func wantsStream(body []byte) bool {
	var peek struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return false
	}
	return peek.Stream
}

func intPtr(v *int) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Limiter returns the rate limiter, or nil before one is attached.
func (s *Service) Limiter() *ratelimit.Limiter { return s.limiter }
