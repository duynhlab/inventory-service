package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/duynhlab/inventory-service/config"
	migrations "github.com/duynhlab/inventory-service/db/migrations"
	seed "github.com/duynhlab/inventory-service/db/seed"
	database "github.com/duynhlab/inventory-service/internal/core"
	"github.com/duynhlab/inventory-service/internal/core/repository"
	grpcv1 "github.com/duynhlab/inventory-service/internal/grpc/v1"
	logicv1 "github.com/duynhlab/inventory-service/internal/logic/v1"
	webv1 "github.com/duynhlab/inventory-service/internal/web/v1"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/httpmw"
	"github.com/duynhlab/pkg/logger/slogx"
	"github.com/duynhlab/pkg/migratex"
	"github.com/duynhlab/pkg/obsx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

func main() {
	ctx := context.Background()
	cfg := config.Load()

	logger := slogx.New(slogx.Config{Level: os.Getenv("LOG_LEVEL")})
	slogx.SetDefault(logger)

	// Subcommands (`migrate`, `seed`) run an embedded SQL set and
	// exit; no args serves the app. They still fail fast on a bad config, but
	// only on the part they use: the mop chart's init container passes DB_* env
	// alone, so validating the serving config here would crash-loop it.
	if len(os.Args) > 1 {
		if err := cfg.ValidateForSubcommand(); err != nil {
			panic("Configuration validation failed: " + err.Error())
		}
		if runSubcommand(os.Args[1], cfg, logger) {
			return
		}
	}

	// The serving path needs everything.
	if err := cfg.Validate(); err != nil {
		panic("Configuration validation failed: " + err.Error())
	}

	logger.Info(ctx, "Service starting",
		slog.String("service.version", cfg.Service.Version),
		slog.String("deployment.environment.name", cfg.Service.Env),
		slog.String("port", cfg.Service.Port),
	)

	pool, err := database.Connect(context.Background(), cfg)
	if err != nil {
		// Fatal (exit 1), not return: exiting 0 here would look like a clean
		// shutdown to Kubernetes instead of a crash to restart and alert on.
		logger.Fatal(ctx, "Failed to connect to database", slogx.Err(err))
	}
	defer pool.Close()
	logger.Info(ctx, "Database connection pool established")

	// RFC-0014: single OTel wiring point — traces per TRACING_ENABLED, OTLP
	// metrics (OTEL_METRICS_ENABLED defaults on, =false is a kill switch),
	// logs behind OTEL_LOGS_ENABLED. The config is built once so the startup
	// log reflects the values obsx actually uses.
	otelCfg := obsx.ConfigFromEnv()
	var tp interface{ Shutdown(context.Context) error }
	obs, err := obsx.SetupObservability(context.Background(), otelCfg)
	if err != nil {
		logger.Warn(ctx, "Failed to initialize OpenTelemetry", slogx.Err(err))
	} else {
		tp = obs
		// The facade reaches OTLP through the global logger provider obsx
		// installed; rebuilding it only wires Flush, so a Fatal record is
		// exported before the process exits.
		logger = slogx.New(slogx.Config{Level: os.Getenv("LOG_LEVEL"), Flush: obs.ForceFlush})
		slogx.SetDefault(logger)
		logger.Info(ctx, "OpenTelemetry initialized",
			slog.Bool("traces", obs.Enabled().Traces),
			slog.Bool("otlp_metrics", obs.Enabled().Metrics),
			slog.Bool("otlp_logs", obs.Enabled().Logs),
			slog.String("endpoint", otelCfg.Endpoint),
			slog.Float64("sample_rate", otelCfg.SampleRate),
		)
	}

	// Initialize Pyroscope profiling via shared obsx helper
	if cfg.Profiling.Enabled {
		stopProfiling, err := obsx.SetupProfiling()
		if err != nil {
			logger.Warn(ctx, "Failed to initialize profiling", slogx.Err(err))
		} else {
			logger.Info(ctx, "Profiling initialized", slog.String("endpoint", cfg.Profiling.Endpoint))
			defer func() {
				if err := stopProfiling(context.Background()); err != nil {
					logger.Error(ctx, "Profiling shutdown error", slogx.Err(err))
				}
			}()
		}
	} else {
		logger.Info(ctx, "Profiling disabled (PROFILING_ENABLED=false)")
	}

	// Internal gRPC server — the service's only business API surface.
	// HTTP :8080 carries operational endpoints (/health, /ready) only.
	availabilitySvc := logicv1.NewAvailabilityService(repository.NewAvailabilityRepository(pool))
	reservationSvc := logicv1.NewReservationService(repository.NewReservationRepository(pool))
	grpcSrv, healthSrv := startGRPC(cfg, logger, availabilitySvc, reservationSvc)

	// Protected Backoffice surface (RFC-0023 slice A): the service's first
	// HTTP business routes, verified in-service and role-gated — the edge's
	// JWT check is coarse, this one is authoritative (ADR-047). ADR-050: the
	// verifier trusts the STAFF realm; a customer-realm token fails here (and
	// at the edge) as wrong-issuer before any role logic.
	verifier, err := authmw.NewVerifier(authmw.Config{
		Issuer:   cfg.OIDCStaffIssuer,
		Audience: cfg.OIDCAudience,
		JWKSURL:  cfg.OIDCStaffJWKSURL,
	})
	if err != nil {
		logger.Fatal(ctx, "JWKS verifier init failed", slogx.Err(err))
	}
	adminHandler := webv1.NewHandler(logicv1.NewAdminService(
		repository.NewAdminReadRepository(pool),
		repository.NewStockCommandRepository(pool),
	))

	var isShuttingDown atomic.Bool
	srv := setupServer(cfg, logger, &isShuttingDown, pool, adminHandler, verifier)
	runGracefulShutdown(cfg, srv, grpcSrv, healthSrv, tp, pool, logger, &isShuttingDown)
}

// runSubcommand handles the `migrate` and `seed` subcommands. It returns true
// when a subcommand was recognised and executed (the caller then exits), or
// false to fall through to serving the app.
//
// `migrate` applies the versioned schema migrations and runs in every
// environment (init container, direct DB host). `seed` applies DEV-ONLY demo
// data and is invoked explicitly — never by `migrate` or the serve path — so
// production databases are never seeded.
//
// The phase-2 `backfill` subcommand (RFC-0021 P2-2) was RETIRED in phase 4
// together with its only data source. It read products.stock_quantity, a column
// frozen at the write cutover and dropped by product migration 000006, and it
// reached the product database with a cross-service read-only grant that phase 4
// revokes. Nothing was left for it to read or a way for it to connect, so it was
// removed rather than kept as a subcommand that can only fail. Recovering a
// missing balance is now an inventory-local operation — seed, or an explicit
// RECEIVE movement — never a copy of product's frozen numbers.
func runSubcommand(cmd string, cfg *config.Config, logger *slogx.Logger) bool {
	ctx := context.Background()
	switch cmd {
	case "migrate":
		if err := migratex.Run(migrations.FS, "sql", cfg.Database.BuildDSN()); err != nil {
			logger.Fatal(ctx, "Schema migration failed", slogx.Err(err))
		}
		logger.Info(ctx, "Schema migrations applied")
		return true
	case "seed":
		// Demo data is DEV-ONLY; only an explicitly-development environment
		// may seed — staging and anything unrecognised are refused too, not
		// just production.
		if !cfg.IsDevelopment() {
			logger.Fatal(ctx, "seed refused — demo data is dev-only (ENV must be development)")
		}
		if err := applySeed(cfg); err != nil {
			logger.Fatal(ctx, "Demo seed failed", slogx.Err(err))
		}
		logger.Info(ctx, "Demo seed data applied")
		return true
	case "backfill":
		// RETIRED in RFC-0021 phase 4 (see the note on runSubcommand). Kept as an
		// explicit refusal, NOT deleted: `default` falls through to serving the app,
		// so without this arm `inventory backfill --apply` would start a full HTTP +
		// gRPC server inside a one-shot Job -- holding product-database credentials,
		// with --apply silently discarded as an unparsed argument, and nothing to
		// reap it. A removed subcommand has to say it was removed; anything else
		// turns a stale runbook line into a running server.
		logger.Fatal(ctx, "`backfill` was retired in RFC-0021 phase 4 — its source column "+
			"products.stock_quantity no longer exists, and the cross-service grant is "+
			"revoked. Recover a balance at inventory: `seed` (dev only) or an explicit "+
			"RECEIVE movement.")
		return true
	default:
		return false
	}
}

// applySeed executes the embedded dev-only seed SQL directly against the
// database. It does NOT use golang-migrate: seeds are idempotent (ON CONFLICT)
// and must not share the schema_migrations version table with the schema
// migrations. Simple query protocol lets each multi-statement seed file run in
// one Exec.
func applySeed(cfg *config.Config) error {
	ctx := context.Background()

	poolCfg, err := pgxpool.ParseConfig(cfg.Database.BuildDSN())
	if err != nil {
		return fmt.Errorf("parse seed DSN: %w", err)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect for seed: %w", err)
	}
	defer pool.Close()

	entries, err := fs.ReadDir(seed.FS, "sql")
	if err != nil {
		return fmt.Errorf("read seed dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		b, readErr := fs.ReadFile(seed.FS, "sql/"+name)
		if readErr != nil {
			return fmt.Errorf("read seed %s: %w", name, readErr)
		}
		if _, execErr := pool.Exec(ctx, string(b)); execErr != nil {
			return fmt.Errorf("apply seed %s: %w", name, execErr)
		}
	}
	return nil
}

// startGRPC starts the internal gRPC server on cfg.GRPC.Port, serving
// InventoryService alongside the HTTP listener (dual-port). gRPC is the
// service's ONLY business surface, so a pod that cannot serve it is useless:
// bind or serve failure exits non-zero instead of leaving a Ready zombie
// that answers /health while every RPC fails. The server uses the shared
// grpcx bootstrap (OpenTelemetry, health, reflection); the returned
// *health.Server lets shutdown flip NOT_SERVING before draining.
func startGRPC(
	cfg *config.Config,
	logger *slogx.Logger,
	availability *logicv1.AvailabilityService,
	reservations *logicv1.ReservationService,
) (*grpc.Server, *health.Server) {
	ctx := context.Background()
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", ":"+cfg.GRPC.Port)
	if err != nil {
		logger.Fatal(ctx, "Failed to listen for gRPC", slog.String("port", cfg.GRPC.Port), slogx.Err(err))
	}

	grpcSrv, healthSrv := grpcx.NewServer(logger.Slog())
	inventoryv1.RegisterInventoryServiceServer(grpcSrv, grpcv1.NewServer(availability, reservations))

	go func() {
		logger.Info(ctx, "Starting gRPC server", slog.String("port", cfg.GRPC.Port))
		// Serve returns nil after Stop/GracefulStop, so Fatal only fires on a
		// real serve failure.
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Fatal(ctx, "gRPC server error", slogx.Err(err))
		}
	}()

	return grpcSrv, healthSrv
}

func setupServer(cfg *config.Config, logger *slogx.Logger, isShuttingDown *atomic.Bool, pool interface {
	Ping(context.Context) error
}, adminHandler *webv1.Handler, verifier *authmw.Verifier) *http.Server {
	// Gin defaults to debug mode; anything but development runs release mode
	// so per-route debug banners stay out of production logs.
	if !cfg.IsDevelopment() {
		gin.SetMode(gin.ReleaseMode)
	}
	// gin.New, not gin.Default: Default installs gin's own logger and
	// recovery, which print the raw path and client address past the facade.
	// httpmw replaces both: the canonical access record and a structured
	// panic record, answered as a 500.
	r := gin.New()
	r.Use(httpmw.Logging(logger.Slog()))
	r.Use(httpmw.Recovery(logger.Slog()))

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if isShuttingDown.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "shutting_down"})
			return
		}
		pingCtx, cancel := context.WithTimeout(c.Request.Context(), 1*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// East-west stays gRPC-only (RFC-0021). The single HTTP business surface
	// is the protected Backoffice group (RFC-0023) — operator traffic through
	// the edge, never service-to-service.
	webv1.RegisterRoutes(r, adminHandler, verifier)

	return &http.Server{
		Addr:              ":" + cfg.Service.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func runGracefulShutdown(
	cfg *config.Config,
	srv *http.Server,
	grpcSrv *grpc.Server,
	healthSrv *health.Server,
	tp interface{ Shutdown(context.Context) error },
	pool interface{ Close() },
	logger *slogx.Logger,
	isShuttingDown *atomic.Bool,
) {
	ctx := context.Background()
	go func() {
		logger.Info(ctx, "Starting inventory service", slog.String("port", cfg.Service.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(ctx, "Failed to start server", slogx.Err(err))
		}
	}()

	logger.ProcessStarted(ctx, slogx.ComponentAPI)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	<-sigCtx.Done()
	logger.Info(ctx, "Shutdown signal received")

	isShuttingDown.Store(true)
	// Flip the gRPC health status at the start of the drain so clients that
	// watch the health service stop picking this instance while in-flight
	// RPCs finish.
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	drainDelay := cfg.GetReadinessDrainDelayDuration()
	if drainDelay > 0 {
		logger.Info(ctx, "Readiness drain delay started", slog.Duration("delay", drainDelay))
		time.Sleep(drainDelay)
	}

	shutdownTimeout := cfg.GetShutdownTimeoutDuration()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	logger.Info(ctx, "Shutting down server...", slog.Duration("timeout", shutdownTimeout))

	outcome := slogx.OutcomeGraceful
	if err := srv.Shutdown(shutdownCtx); err != nil {
		outcome = slogx.OutcomeError
		logger.Error(ctx, "HTTP server shutdown error", slogx.Err(err))
	} else {
		logger.Info(ctx, "HTTP server shutdown complete")
	}

	// GracefulStop waits for in-flight RPCs but has no deadline of its own; a
	// stuck stream would block past the pod's terminationGracePeriod, so fall
	// back to a hard Stop after 10s.
	stopped := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		logger.Info(ctx, "gRPC server shutdown complete")
	case <-time.After(10 * time.Second):
		grpcSrv.Stop()
		<-stopped
		outcome = slogx.OutcomeError
		logger.Warn(ctx, "gRPC server force-stopped after graceful timeout")
	}

	pool.Close()
	logger.Info(ctx, "Database pool closed")

	// process.stopped goes out BEFORE the OTel SDK shuts down: a record
	// emitted after it is dropped rather than exported.
	logger.ProcessStopped(ctx, slogx.ComponentAPI, outcome)

	// Shutdown the OTel SDK — flushes pending spans plus any OTLP
	// metrics/logs providers built behind the RFC-0014 flags.
	if tp != nil {
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Error(ctx, "OpenTelemetry shutdown error", slogx.Err(err))
		} else {
			logger.Info(ctx, "OpenTelemetry shutdown complete")
		}
	}

	logger.Info(ctx, "Graceful shutdown complete")
}
