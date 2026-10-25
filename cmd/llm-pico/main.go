// Command llm-pico is a lightweight LLM proxy exposing an OpenAI-compatible
// API, configured entirely from files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/arkan/llm-pico/internal/auth"
	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/proxy"
	"github.com/arkan/llm-pico/internal/ratelimit"
	"github.com/arkan/llm-pico/internal/server"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

func main() {
	var (
		configDir = flag.String("config-dir", ".", "directory holding keys.env, provider.yaml, models.yaml, aliases.yaml")
		host      = flag.String("host", "0.0.0.0", "listen host")
		port      = flag.Int("port", 4000, "listen port")
		verbose   = flag.Bool("verbose", false, "enable debug logging")
		showVer   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("llm-pico %s (%s/%s)\n", server.Version, runtime.GOOS, runtime.GOARCH)
		return
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(runOptions{
		configDir: *configDir, host: *host, port: *port, log: log,
	}); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

type runOptions struct {
	configDir string
	host      string
	port      int
	log       *slog.Logger
}

func run(opts runOptions) error {
	cfg, err := config.Load(opts.configDir)
	if err != nil {
		return err
	}

	reg, err := cfg.Registry()
	if err != nil {
		return fmt.Errorf("build provider registry: %w", err)
	}

	limiter := ratelimit.New()
	limiter.Start()
	defer limiter.Stop()

	svc := proxy.NewService(cfg, reg, opts.log)
	svc.SetLimiter(limiter)
	svc.SetAudit(auth.NewAuditLog(filepath.Join(opts.configDir, "audit.jsonl")))

	httpServer := startHTTP(opts, svc)

	// A misconfigured upstream should not stop serving the healthy ones, so
	// key and endpoint problems are probed in the background and logged only.
	go probeUpstreams(svc, opts.log)

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	go func() {
		for range reload {
			newCfg, err := config.Load(opts.configDir)
			if err != nil {
				opts.log.Error("config reload failed; keeping previous configuration", "error", err)
				continue
			}
			newReg, err := newCfg.Registry()
			if err != nil {
				opts.log.Error("provider registry reload failed; keeping previous configuration", "error", err)
				continue
			}
			svc.Reload(newCfg, newReg)
			opts.log.Info("configuration reloaded")
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		opts.log.Info("listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		opts.log.Info("shutting down", "signal", sig.String())
	}
	signal.Stop(reload)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stop accepting new connections, then drain in-flight requests so
	// streaming responses finish rather than being cut off.
	if err := httpServer.Shutdown(ctx); err != nil {
		opts.log.Warn("graceful shutdown timed out; forcing close", "error", err)
	}
	if !svc.BeginDrain(30 * time.Second) {
		opts.log.Warn("drain timed out with requests still in flight")
	}
	opts.log.Info("shutdown complete")
	return nil
}

// startHTTP builds the listener and the server without serving yet.
func startHTTP(opts runOptions, svc *proxy.Service) *http.Server {
	srv := server.New(server.Options{
		Proxy:        svc,
		Logger:       opts.log,
		DrainTimeout: 30 * time.Second,
	})
	return &http.Server{
		Addr:    fmt.Sprintf("%s:%d", opts.host, opts.port),
		Handler: srv,
		// Streaming responses are long-lived by design, so no write timeout is
		// set here; the read header timeout still bounds slow-loris clients.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}
}

// probeUpstreams probes each configured slot once at boot and then backs off,
// logging failures without ever affecting request handling.
func probeUpstreams(svc *proxy.Service, log *slog.Logger) {
	backoff := 5 * time.Second
	const maxBackoff = 5 * time.Minute
	for {
		cfg := svc.Config()
		reg := svc.Providers
		var failed int
		for _, p := range cfg.Providers() {
			adapter := reg.AdapterFor(p.Adapter, p.BaseURL)
			if adapter == nil {
				continue
			}
			for _, slot := range cfg.ModelList {
				key := slot.ModelParams.APIKey
				if key == "" && len(slot.ModelParams.APIKeys) > 0 {
					key = slot.ModelParams.APIKeys[0]
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := adapter.Probe(ctx, providers.Request{
					Model:   slot.ModelParams.Model,
					APIKey:  key,
					BaseURL: slot.ModelParams.APIBase,
				})
				cancel()
				if err != nil {
					failed++
					log.Warn("upstream probe failed", "slot", slot.Provider, "model", slot.ModelName, "error", err)
				}
			}
		}
		if failed == 0 {
			log.Info("upstream probes passed")
			return
		}
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}
