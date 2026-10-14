// Command llm-pico is a lightweight LLM proxy exposing an OpenAI-compatible
// API backed by a single SQLite database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/arkan/llm-pico/internal/auth"
	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/proxy"
	"github.com/arkan/llm-pico/internal/ratelimit"
	"github.com/arkan/llm-pico/internal/server"
	"github.com/arkan/llm-pico/internal/store"
)

func main() {
	var (
		host       = flag.String("host", "0.0.0.0", "listen host")
		port       = flag.Int("port", 4000, "listen port")
		dbPath     = flag.String("db", "llm-pico.db", "SQLite database path")
		masterKey  = flag.String("master-key", "", "master key to set on first boot (also read from LLM_PICO_MASTER_KEY)")
		workers    = flag.Int("workers", 1, "number of worker processes")
		verbose    = flag.Bool("verbose", false, "enable debug logging")
		trustProxy = flag.Bool("trust-proxy-headers", false, "honour X-Forwarded-For (only enable behind a proxy that sets it)")
		probe      = flag.Bool("probe-models", false, "probe model capabilities on startup")
		showVer    = flag.Bool("version", false, "print the version and exit")
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
		host: *host, port: *port, dbPath: *dbPath, masterKey: *masterKey,
		workers: *workers, trustProxy: *trustProxy, probe: *probe, log: log,
	}); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

type runOptions struct {
	host       string
	port       int
	dbPath     string
	masterKey  string
	workers    int
	trustProxy bool
	probe      bool
	log        *slog.Logger
}

func run(opts runOptions) error {
	if opts.workers < 1 {
		opts.workers = 1
	}

	// Multiple workers means multiple processes sharing one SQLite file. WAL
	// mode makes concurrent readers safe, but in-memory state - the rate
	// limiter, circuit breakers, health scores - is per process and would
	// silently diverge. Rather than let that produce inconsistent behaviour
	// that is hard to reason about, workers above one are refused explicitly.
	// The Python implementation accepted --workers N, logged a message and
	// then ignored it entirely.
	if opts.workers > 1 {
		return fmt.Errorf(
			"--workers %d is not supported: rate limits, circuit breakers and health scores are held in memory and would diverge between processes. "+
				"Run a single process behind a load balancer, or raise this as an issue if you need multi-process support",
			opts.workers)
	}

	absDB, err := filepath.Abs(opts.dbPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}

	db, err := store.Open(absDB)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	opts.log.Info("database ready", "path", absDB)

	cfg, err := config.Load(context.Background(), db)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// A master key supplied on first boot is persisted so every later start
	// finds it, which is what makes the dashboard usable after a restart.
	if cfg.GeneralSettings.MasterKey == "" {
		key := opts.masterKey
		if key == "" {
			key = os.Getenv("LLM_PICO_MASTER_KEY")
		}
		if key != "" {
			if err := config.SaveSettings(context.Background(), db, map[string]any{"master_key": key}); err != nil {
				return fmt.Errorf("persist master key: %w", err)
			}
			cfg.GeneralSettings.MasterKey = key
			opts.log.Info("master key set from configuration")
		}
	}

	if cfg.GeneralSettings.MasterKey == "" {
		opts.log.Warn("no master key configured; open the dashboard to finish setup")
	} else {
		opts.log.Info("instance configured", "models", len(cfg.ModelList))
	}

	limiter := ratelimit.New(db)
	limiter.Start()
	defer limiter.Stop(context.Background())

	svc := proxy.NewService(db, cfg, providers.NewRegistry(nil), opts.log)
	svc.SetLimiter(limiter)
	svc.SetAudit(auth.NewAuditLog(filepath.Join(filepath.Dir(absDB), "audit.jsonl")))

	if opts.probe {
		go probeCapabilities(svc, opts.log)
	}
	go pruneLogs(svc, cfg, opts.log)

	srv := server.New(server.Options{
		DB:                db,
		Proxy:             svc,
		Logger:            opts.log,
		TrustProxyHeaders: opts.trustProxy,
		DrainTimeout:      30 * time.Second,
	})

	httpServer := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", opts.host, opts.port),
		Handler: srv,
		// Streaming responses are long-lived by design, so no write timeout is
		// set here; the read header timeout still bounds slow-loris clients.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}

	errCh := make(chan error, 1)
	go func() {
		opts.log.Info("listening", "addr", httpServer.Addr, "dashboard", "http://localhost:"+fmt.Sprint(opts.port)+"/admin/dashboard")
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

// probeCapabilities detects what each configured model supports and stores the
// result, so the dashboard can show accurate capability badges.
func probeCapabilities(svc *proxy.Service, log *slog.Logger) {
	time.Sleep(2 * time.Second) // let the server finish binding

	ctx := context.Background()
	cfg := svc.Config()
	for _, entry := range cfg.ModelList {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := svc.ProbeCapabilities(ctx, entry); err != nil {
			log.Debug("capability probe failed", "model", entry.ModelName, "error", err)
		}
	}
	log.Info("capability probing complete")
}

// pruneLogs deletes usage and admin rows past their retention windows.
func pruneLogs(svc *proxy.Service, cfg *config.Config, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	first := true
	for {
		if !first {
			<-ticker.C
		}
		first = false

		usageRows, adminRows, err := svc.PruneLogs()
		if err != nil {
			log.Error("log pruning failed", "error", err)
			continue
		}
		if usageRows+adminRows > 0 {
			log.Info("pruned old logs", "usage_rows", usageRows, "admin_rows", adminRows)
		}
	}
}
