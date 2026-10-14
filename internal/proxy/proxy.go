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
	"github.com/arkan/llm-pico/internal/org"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/ratelimit"
	"github.com/arkan/llm-pico/internal/router"
	"github.com/arkan/llm-pico/internal/store"
	"github.com/arkan/llm-pico/internal/usage"
)

// RouteType selects which upstream operation a request performs.
type RouteType string

const (
	RouteChat        RouteType = "chat"
	RouteCompletions RouteType = "completions"
	RouteEmbeddings  RouteType = "embeddings"
	RouteSTT         RouteType = "stt"
	RouteTTS         RouteType = "tts"
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
// Configuration and the routing table are held behind atomic pointers so a
// hot reload swaps them without blocking in-flight requests and without any
// request observing a half-applied configuration.
type Service struct {
	DB        *store.Store
	limiter   *ratelimit.Limiter
	Providers *providers.Registry
	Latency   *observability.LatencyTracker
	Events    *observability.Bus
	Audit     *auth.AuditLog
	Log       *slog.Logger

	config atomic.Pointer[config.Config]
	routes atomic.Pointer[router.Router]
	mode   atomic.Value // DegradationMode

	draining atomic.Bool
	inFlight sync.WaitGroup

	// queue bounds concurrent requests in fallback_only mode.
	sem chan struct{}
}

// NewService builds a Service from a configuration snapshot.
func NewService(db *store.Store, cfg *config.Config, providers *providers.Registry, logger *slog.Logger) *Service {
	s := &Service{
		DB:        db,
		Providers: providers,
		Latency:   observability.NewLatencyTracker(1000),
		Events:    observability.NewBus(256),
		Log:       logger,
		sem:       make(chan struct{}, 512),
	}
	s.config.Store(cfg)
	s.routes.Store(router.New(cfg))
	s.mode.Store(DegradationNormal)
	return s
}

// SetLimiter attaches the rate limiter once the database is open.
func (s *Service) SetLimiter(l *ratelimit.Limiter) { s.limiter = l }

// SetAudit attaches the audit log.
func (s *Service) SetAudit(a *auth.AuditLog) { s.Audit = a }

// Config returns the current configuration snapshot.
func (s *Service) Config() *config.Config { return s.config.Load() }

// Router returns the current routing table.
func (s *Service) Router() *router.Router { return s.routes.Load() }

// Reload rebuilds the configuration and routing table from the database and
// swaps them in atomically.
//
// This replaces the Python implementation's approach, which assigned to
// app_state.router while other requests were reading it, and which restarted
// the whole process with os.execve if the reload failed. Here a failed reload
// leaves the previous configuration serving traffic and reports the error.
func (s *Service) Reload(ctx context.Context) error {
	cfg, err := config.Load(ctx, s.DB)
	if err != nil {
		return err
	}
	// Build the new routing table before publishing either value, so a
	// failure here cannot leave config and router inconsistent.
	routes := router.New(cfg)
	s.config.Store(cfg)
	s.routes.Store(routes)
	return nil
}

// Mode returns the current degradation mode.
func (s *Service) Mode() DegradationMode {
	m, _ := s.mode.Load().(DegradationMode)
	return m
}

// SetMode changes the degradation mode.
func (s *Service) SetMode(m DegradationMode) { s.mode.Store(m) }

// BeginDrain stops admitting new requests and waits for in-flight ones to
// finish, up to a deadline.
//
// This replaces the Python implementation's 0.5-second polling loop over a
// shared set, which could not observe requests that had not yet registered.
func (s *Service) BeginDrain(timeout time.Duration) bool {
	s.draining.Store(true)
	done := make(chan struct{})
	go func() {
		s.inFlight.Wait()
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
	// MasterKey attributes usage for admin-initiated calls, which have no key.
	MasterKey string
}

// Dispatch resolves a model, enforces limits, calls upstream, and accounts.
//
// Retries walk outward through credentials for the model, then the team's
// model chain, then the model's configured failover target. Every retry is
// bounded by RouterSettings.NumRetries.
func (s *Service) Dispatch(ctx context.Context, in DispatchInput) (*http.Response, error) {
	if s.Draining() {
		return nil, httpx.Draining()
	}

	s.inFlight.Add(1)
	defer s.inFlight.Done()

	cfg := s.Config()
	r := s.Router()

	tracer := observability.NewTracer(newRequestID())
	// Tracing is diagnostic; a failure to persist must not affect the client.
	defer tracer.Save(context.WithoutCancel(ctx), s.DB)

	if err := s.admit(ctx, in, cfg); err != nil {
		return nil, err
	}

	candidates := s.candidates(ctx, in, r)
	var (
		lastErr error
		tried   []string
	)

	for i, candidate := range candidates {
		tried = append(tried, candidate)
		resp, err := s.attempt(ctx, attemptCtx{
			tracer: tracer,
			cfg:    cfg,
			router: r,
		}, in, candidate)
		if err == nil {
			if i > 0 && resp != nil {
				httpx.ApplyHeader(resp.Header, httpx.ChainHeaders(candidate, tried, i))
			}
			return resp, nil
		}
		lastErr = err

		// Budget and rate-limit rejections are conditions of the caller, not
		// the upstream: trying another model would spend more of the same
		// budget to reach the same outcome.
		if isTerminal(err) {
			break
		}
		// A model is only cooled down when its upstream actually failed. One
		// key exhausting its rate limit says nothing about the model's health,
		// and cooling it down would make a healthy model unavailable to every
		// other caller.
		if isClientRejection(err) {
			break
		}
		if i == len(candidates)-1 {
			r.RecordModelFailure(candidate)
		}
	}

	if lastErr == nil {
		lastErr = httpx.Upstream(http.StatusBadGateway, "all upstream providers failed")
	}
	return nil, httpx.FromError(lastErr)
}

// attemptCtx is the immutable context of one dispatched request.
type attemptCtx struct {
	tracer *observability.Tracer
	cfg    *config.Config
	router *router.Router
}

// admit applies the current degradation mode before any work is done.
func (s *Service) admit(ctx context.Context, in DispatchInput, cfg *config.Config) error {
	switch s.Mode() {
	case DegradationReject:
		return httpx.Degraded("service degraded: requests temporarily rejected")
	case DegradationFallbackOnly:
		// Only admit a bounded number of concurrent requests, so a provider
		// that is failing can be given room to recover.
		select {
		case s.sem <- struct{}{}:
			return nil
		case <-ctx.Done():
			return httpx.UpstreamTimeout("request cancelled while queued")
		default:
			return httpx.Degraded("service degraded: concurrency limit reached")
		}
	default:
		return nil
	}
}

// release frees a slot taken by admit.
func (s *Service) release() {
	if s.Mode() == DegradationFallbackOnly {
		select {
		case <-s.sem:
		default:
		}
	}
}

// candidates builds the ordered list of model names to try.
func (s *Service) candidates(ctx context.Context, in DispatchInput, r *router.Router) []string {
	out := []string{in.Model}

	// The team chain lets one client-facing model name stand in for an ordered
	// set of alternatives, each with its own fallbacks.
	if in.Identity != nil && in.Identity.UserID != nil {
		user, team, err := org.Resolve(ctx, s.DB, *in.Identity.UserID)
		if err == nil && team != nil && len(team.ModelChain) > 0 {
			rest := team.ModelChain
			if idx := indexOf(team.ModelChain, in.Model); idx >= 0 {
				rest = team.ModelChain[idx+1:]
			}
			for _, m := range rest {
				if m != "" && !contains(out, m) {
					out = append(out, m)
				}
			}
		}
		_ = user
	}

	// The legacy single failover target is the lowest-priority alternative.
	if target, err := r.Resolve(in.Model); err == nil && target.Model.FailoverModel != "" {
		if !contains(out, target.Model.FailoverModel) {
			out = append(out, target.Model.FailoverModel)
		}
	}
	return out
}

// reservation is the per-request rate-limit accounting, threaded through the
// attempt so a retry loop cannot double-reserve.
type reservation struct {
	headers    http.Header
	tokens     int64
	reconciled bool
}

// attempt makes the configured number of tries against a single model.
func (s *Service) attempt(ctx context.Context, ac attemptCtx, in DispatchInput, model string) (*http.Response, error) {
	maxTries := max(1, ac.cfg.RouterSettings.NumRetries+1)
	var (
		res       *reservation
		lastErr   error
		tracedOne bool
	)

	for try := 0; try < maxTries; try++ {
		target, err := ac.router.Resolve(model)
		if err != nil {
			var cooldown *router.CooldownError
			if errors.As(err, &cooldown) {
				return nil, httpx.RateLimited(
					fmt.Sprintf("all keys for model '%s' are rate-limited", model),
					int(cooldown.RetryAfter.Seconds())+1,
				)
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, httpx.NotFound(fmt.Sprintf("model '%s' not available", model))
		}

		if !tracedOne {
			ac.tracer.Add("resolve:"+model, model, target.ProviderSlug, "ok", "", time.Time{})
			tracedOne = true
		}

		if err := checkCapability(in, target); err != nil {
			return nil, err
		}

		// Limits are evaluated once per request, not once per retry, so a
		// retry loop cannot re-reserve the same tokens.
		if res == nil {
			if err := s.checkBudgets(ctx, in, target); err != nil {
				return nil, err
			}
			res, err = s.reserve(ctx, in, target)
			if err != nil {
				return nil, err
			}
		}

		adapter := s.Providers.AdapterFor(target.ProviderSlug, target.APIBase)
		resp, finalize, err := s.call(ctx, in, target, adapter, res)

		if err == nil {
			ac.router.RecordSuccess(target)
			ac.tracer.Add("proxy:"+model, model, target.ProviderSlug, "ok", "", time.Time{})
			if resp != nil && res != nil {
				httpx.ApplyHeader(resp.Header, res.headers)
				resp.Header.Set("X-Request-Id", ac.tracer.ID())
			}
			// For a buffered response this settles the accounting now; for a
			// stream it settles when the copy goroutine finishes.
			finalize(target, nil)
			return resp, nil
		}

		var apiErr *httpx.Error
		if errors.As(err, &apiErr) && isTerminal(err) {
			ac.router.RecordFailure(target, apiErr.Status)
			ac.tracer.Add("error:"+model, model, target.ProviderSlug, "fail",
				fmt.Sprint(apiErr.Status), time.Time{})
			refund(ctx, s, in, res)
			return nil, err
		}

		status := statusOf(err)
		ac.router.RecordFailure(target, status)
		ac.tracer.Add("error:"+model, model, target.ProviderSlug, "fail",
			fmt.Sprint(status), time.Time{})
		lastErr = err
	}

	// Every attempt failed: release the reservation so the caller is not
	// charged for work that never happened.
	refund(ctx, s, in, res)
	return nil, lastErr
}

// checkCapability rejects a request the target model cannot serve, before
// spending an upstream call.
func checkCapability(in DispatchInput, target *router.Target) error {
	entry := target.Model
	switch in.RouteType {
	case RouteEmbeddings:
		if !entry.Embeddings {
			return httpx.BadRequest(fmt.Sprintf("model '%s' does not support embeddings", in.Model))
		}
	case RouteSTT:
		if !entry.STT {
			return httpx.BadRequest(fmt.Sprintf("model '%s' does not support speech-to-text", in.Model))
		}
	case RouteTTS:
		if !entry.TTS {
			return httpx.BadRequest(fmt.Sprintf("model '%s' does not support text-to-speech", in.Model))
		}
	}
	if in.RouteType == RouteChat && providers.HasImageInput(in.Body) && !entry.Images {
		return httpx.BadRequest(fmt.Sprintf("model '%s' does not support image inputs", in.Model))
	}
	return nil
}

// checkBudgets enforces key, user and team monthly budgets before dispatch.
func (s *Service) checkBudgets(ctx context.Context, in DispatchInput, target *router.Target) error {
	if in.Identity == nil || in.Identity.Role != auth.RoleUser {
		return nil
	}
	entry := target.Model
	// Project against the reservation, not zero, so a large max_tokens cannot
	// overshoot the budget within the same month.
	estimate := usage.Cost(len(in.Body)/4, in.MaxTokens, entry.CostPer1MIn, entry.CostPer1MOut)
	if estimate == nil {
		return nil
	}

	if msg, err := usage.CheckKeyBudget(ctx, s.DB, in.Identity.KeyHash, *estimate); err != nil {
		return err
	} else if msg != "" {
		return httpx.BudgetExceeded(msg)
	}

	if in.Identity.UserID == nil {
		return nil
	}
	if msg, err := org.CheckUserBudget(ctx, s.DB, *in.Identity.UserID, *estimate); err != nil {
		return err
	} else if msg != "" {
		return httpx.BudgetExceeded(msg)
	}

	_, team, err := org.Resolve(ctx, s.DB, *in.Identity.UserID)
	if err != nil {
		return err
	}
	if team != nil {
		if msg, err := org.CheckTeamBudget(ctx, s.DB, team.ID, *estimate); err != nil {
			return err
		} else if msg != "" {
			return httpx.BudgetExceeded(msg)
		}
	}
	return nil
}

// reserve consumes capacity from both the user-level and model-level limit
// sets and returns the headers to report back.
func (s *Service) reserve(ctx context.Context, in DispatchInput, target *router.Target) (*reservation, error) {
	if s.limiter == nil {
		return &reservation{}, nil
	}
	keyHash, _ := in.Identity.Ledger(in.MasterKey)
	entry := target.Model

	// Four characters per token is the usual English approximation; the real
	// figure is reconciled against provider-reported usage afterwards.
	tokens := int64(max(1, len(in.Body)/4) + in.MaxTokens)

	userLimits := in.Identity.LimiterLimits()
	modelLimits := ratelimit.Limits{
		ratelimit.RPM: intPtr(entry.RPM),
		ratelimit.RPD: intPtr(entry.RPD),
		ratelimit.TPM: intPtr(entry.TPM),
		ratelimit.TPD: intPtr(entry.TPD),
		ratelimit.ASH: intPtr(entry.ASH),
		ratelimit.ASD: intPtr(entry.ASD),
	}

	levels := []struct {
		level  ratelimit.Level
		limits ratelimit.Limits
	}{
		{ratelimit.LevelUser, userLimits},
		{ratelimit.LevelModel, modelLimits},
	}

	for _, l := range levels {
		if !l.limits.HasAny() {
			continue
		}
		if rejection := s.limiter.CheckAndReserve(keyHash, in.Model, l.level, l.limits, tokens); rejection != nil {
			return nil, httpx.RateLimited(
				fmt.Sprintf("rate limit exceeded: %s", rejection.Window),
				int(rejection.RetryAfter.Seconds())+1,
			)
		}
	}

	headers := httpx.RateLimitHeaders(s.limiter, keyHash, in.Model, ratelimit.LevelUser, userLimits)
	httpx.ApplyHeader(headers, httpx.RateLimitHeaders(s.limiter, keyHash, in.Model, ratelimit.LevelModel, modelLimits))
	return &reservation{headers: headers, tokens: tokens}, nil
}

// settle returns unused reservation to the limiter. It runs at most once, since
// a second call would refund capacity that was never taken.
func (s *Service) settle(ctx context.Context, in DispatchInput, res *reservation, actual int64) {
	if s.limiter == nil || res == nil || res.reconciled {
		return
	}
	res.reconciled = true
	keyHash, _ := in.Identity.Ledger(in.MasterKey)
	s.limiter.Reconcile(keyHash, in.Model, ratelimit.LevelUser, actual, res.tokens)
}

func refund(ctx context.Context, s *Service, in DispatchInput, res *reservation) {
	if res == nil {
		return
	}
	s.settle(ctx, in, res, 0)
}

// callUsage is what a completed upstream call cost.
type callUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Latency          time.Duration
}

// finalize settles accounting for one call. For a buffered response it runs
// inline; for a stream the copy goroutine runs it once the body is drained.
func finalizeFn(s *Service, in DispatchInput, res *reservation, u *callUsage, target *router.Target, onDone func()) func(*router.Target, error) {
	return func(finalTarget *router.Target, err error) {
		defer onDone()

		keyHash, keyPrefix := in.Identity.Ledger(in.MasterKey)
		entry := finalTarget.Model
		cost := usage.Cost(u.PromptTokens, u.CompletionTokens, entry.CostPer1MIn, entry.CostPer1MOut)

		total := u.TotalTokens
		if total == 0 && res != nil {
			// A provider reporting no usage is assumed to have consumed
			// exactly what was reserved, which keeps the limiter from drifting
			// in either direction.
			total = int(res.tokens)
		}

		status := http.StatusOK
		errorText := ""
		errorType := ""
		if err != nil {
			status = statusOf(err)
			errorText = truncate(err.Error(), 500)
			errorType = usage.Classify(status, errorText)
		}

		_, _ = usage.Log(context.WithoutCancel(context.Background()), s.DB, usage.Record{
			KeyHash:          keyHash,
			KeyPrefix:        keyPrefix,
			ModelName:        in.Model,
			Provider:         finalTarget.ProviderSlug,
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      total,
			LatencyMS:        int(u.Latency.Milliseconds()),
			StatusCode:       status,
			Error:            errorText,
			ErrorType:        errorType,
			CostUSD:          cost,
		})

		costValue := 0.0
		if cost != nil {
			costValue = *cost
		}
		s.Events.Publish(observability.NewEvent(keyPrefix, in.Model, finalTarget.ProviderSlug,
			u.PromptTokens, u.CompletionTokens, total,
			int(u.Latency.Milliseconds()), status, costValue))

		if err == nil {
			s.settle(context.Background(), in, res, int64(total))
		} else {
			refund(context.Background(), s, in, res)
		}
	}
}

// call performs the upstream request and returns the response plus a
// finalize function that settles accounting exactly once.
func (s *Service) call(
	ctx context.Context,
	in DispatchInput,
	target *router.Target,
	adapter providers.Adapter,
	res *reservation,
) (*http.Response, func(*router.Target, error), error) {
	req := providers.Request{
		Body:    in.Body,
		Model:   target.Model.ModelParams.Model,
		APIKey:  target.APIKey,
		BaseURL: target.APIBase,
	}

	start := time.Now()
	s.release()

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
			return s.callStreaming(ctx, in, target, adapter, req, res, start)
		}
		resp, err = adapter.Complete(ctx, req)
		u = &callUsage{}
	}

	if err != nil {
		return nil, nil, upstreamError(err, target.ProviderSlug)
	}

	if in.RouteType == RouteChat || in.RouteType == RouteCompletions {
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
	return out, finalizeFn(s, in, res, u, target, func() {}), nil
}

// callStreaming pipes the upstream stream to the client and settles accounting
// when the body is drained.
func (s *Service) callStreaming(
	ctx context.Context,
	in DispatchInput,
	target *router.Target,
	adapter providers.Adapter,
	req providers.Request,
	res *reservation,
	start time.Time,
) (*http.Response, func(*router.Target, error), error) {
	req.Stream = true
	stream, err := adapter.Stream(ctx, req)
	if err != nil {
		return nil, nil, upstreamError(err, target.ProviderSlug)
	}

	pr, pw := io.Pipe()
	info := &callUsage{Latency: time.Since(start)}
	settle := sync.Once{}

	finalize := func(finalTarget *router.Target, err error) {
		settle.Do(func() {
			finalizeFn(s, in, res, info, target, func() {})(finalTarget, err)
		})
	}

	go func() {
		var last *providers.Usage
		for {
			chunk, err := stream.Next()
			if err != nil {
				if err != io.EOF {
					_ = pw.CloseWithError(err)
					finalize(target, err)
					return
				}
				break
			}
			if chunk.Usage != nil {
				last = chunk.Usage
			}
			if chunk.Done || len(chunk.Data) == 0 {
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
			info.PromptTokens = last.PromptTokens
			info.CompletionTokens = last.CompletionTokens
			info.TotalTokens = last.TotalTokens
		}
		info.Latency = time.Since(start)
		s.Latency.Record(target.Model.ModelName, target.ProviderSlug, int(info.Latency.Milliseconds()))
		finalize(target, nil)
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
	}, finalize, nil
}

// bufferedUsage extracts token counts from a non-streaming response body.
func bufferedUsage(body []byte) *callUsage {
	u := &callUsage{}
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return u
	}
	u.PromptTokens = payload.Usage.PromptTokens
	u.CompletionTokens = payload.Usage.CompletionTokens
	u.TotalTokens = payload.Usage.TotalTokens
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
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
	if apiErr.Type == httpx.TypeBudgetExceeded {
		return true
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
	return apiErr.Type == httpx.TypeRateLimited || apiErr.Type == httpx.TypeBudgetExceeded
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

func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}

func contains(list []string, v string) bool { return indexOf(list, v) >= 0 }

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

// ProbeCapabilities detects and stores what a model supports. Failures are
// returned for logging but are not fatal: a model that cannot be probed is
// still usable.
func (s *Service) ProbeCapabilities(ctx context.Context, entry config.ModelEntry) error {
	slug := entry.ProviderSlug()
	adapter := s.Providers.AdapterFor(slug, entry.ModelParams.APIBase)

	apiKey := ""
	if keys := entry.Keys(); len(keys) > 0 {
		apiKey = keys[0]
	}

	caps, err := adapter.Probe(ctx, providers.Request{
		Model:   entry.ModelParams.Model,
		APIKey:  apiKey,
		BaseURL: entry.ModelParams.APIBase,
	})
	if err != nil {
		return err
	}

	caps.ProbedAt = store.NowISO()
	_, err = s.DB.Exec(ctx, `
		INSERT OR REPLACE INTO model_capabilities
		  (model_id, supports_tools, supports_vision, supports_json, supports_stream,
		   max_context, max_output, probed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.DBModelID, boolToInt(caps.SupportsTools), boolToInt(caps.SupportsVision),
		boolToInt(caps.SupportsJSON), boolToInt(caps.SupportsStream),
		caps.MaxContext, caps.MaxOutput, caps.ProbedAt)
	return err
}

// PruneLogs deletes usage and admin rows older than their retention windows,
// returning how many rows of each were removed.
func (s *Service) PruneLogs() (usageRows, adminRows int, err error) {
	ctx := context.Background()
	cfg := s.Config()
	now := time.Now().UTC()

	usageCutoff := now.AddDate(0, 0, -cfg.GeneralSettings.UsageLogRetentionDays)
	adminCutoff := now.AddDate(0, 0, -cfg.GeneralSettings.AdminLogRetentionDays)

	if res, err := s.DB.Exec(ctx, `DELETE FROM usage_log WHERE created_at < ?`,
		usageCutoff.Format("2006-01-02T15:04:05")); err != nil {
		return 0, 0, err
	} else if n, _ := res.RowsAffected(); n > 0 {
		usageRows = int(n)
	}

	if res, err := s.DB.Exec(ctx, `DELETE FROM admin_log WHERE created_at < ?`,
		adminCutoff.Format("2006-01-02T15:04:05")); err != nil {
		return usageRows, 0, err
	} else if n, _ := res.RowsAffected(); n > 0 {
		adminRows = int(n)
	}
	return usageRows, adminRows, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
