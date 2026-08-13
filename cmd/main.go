package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	"github.com/duynhlab/inventory-service/middleware"
	"github.com/duynhlab/pkg/authmw"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/logger/zapx"
	"github.com/duynhlab/pkg/migratex"
	"github.com/duynhlab/pkg/obsx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

func main() {
	cfg := config.Load()

	logger, err := zapx.New(os.Getenv("LOG_LEVEL"))
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() { _ = logger.Sync() }()

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

	logger.Info("Service starting",
		zap.String("service", cfg.Service.Name),
		zap.String("version", cfg.Service.Version),
		zap.String("env", cfg.Service.Env),
		zap.String("port", cfg.Service.Port),
	)

	pool, err := database.Connect(context.Background(), cfg)
	if err != nil {
		// Fatal (exit 1), not return: exiting 0 here would look like a clean
		// shutdown to Kubernetes instead of a crash to restart and alert on.
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer pool.Close()
	logger.Info("Database connection pool established")

	// RFC-0014: single OTel wiring point — traces per TRACING_ENABLED, OTLP
	// metrics (OTEL_METRICS_ENABLED defaults on, =false is a kill switch),
	// logs behind OTEL_LOGS_ENABLED. The config is built once so the startup
	// log reflects the values obsx actually uses.
	otelCfg := obsx.ConfigFromEnv()
	middleware.SetServiceName(otelCfg.ServiceName)
	var tp interface{ Shutdown(context.Context) error }
	obs, err := obsx.SetupObservability(context.Background(), otelCfg)
	if err != nil {
		logger.Warn("Failed to initialize OpenTelemetry", zap.Error(err))
	} else {
		tp = obs
		// RFC-0014 P4: tee application logs into the OTLP pipeline. ZapCore
		// returns a NopCore when OTEL_LOGS_ENABLED is off, so the tee is
		// unconditional; the min level mirrors the stdout core so debug
		// lines never leave the pod on an info-level service.
		minLevel, err := zapcore.ParseLevel(os.Getenv("LOG_LEVEL"))
		if err != nil {
			minLevel = zapcore.InfoLevel
		}
		logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			return zapcore.NewTee(c, obs.ZapCore(otelCfg.ServiceName, minLevel))
		}))
		logger.Info("OpenTelemetry initialized",
			zap.Bool("traces", obs.TracerProvider != nil),
			zap.Bool("otlp_metrics", obs.MeterProvider != nil),
			zap.Bool("otlp_logs", obs.LoggerProvider != nil),
			zap.String("endpoint", otelCfg.Endpoint),
			zap.Float64("sample_rate", otelCfg.SampleRate),
		)
	}

	// Initialize Pyroscope profiling via shared obsx helper
	if cfg.Profiling.Enabled {
		stopProfiling, err := obsx.SetupProfiling()
		if err != nil {
			logger.Warn("Failed to initialize profiling", zap.Error(err))
		} else {
			logger.Info("Profiling initialized", zap.String("endpoint", cfg.Profiling.Endpoint))
			defer func() {
				if err := stopProfiling(context.Background()); err != nil {
					logger.Error("Profiling shutdown error", zap.Error(err))
				}
			}()
		}
	} else {
		logger.Info("Profiling disabled (PROFILING_ENABLED=false)")
	}

	// Internal gRPC server — the service's only business API surface.
	// HTTP :8080 carries operational endpoints (/health, /ready) only.
	availabilitySvc := logicv1.NewAvailabilityService(repository.NewAvailabilityRepository(pool))
	reservationSvc := logicv1.NewReservationService(repository.NewReservationRepository(pool), logicv1.WithLogger(logger))
	grpcSrv, healthSrv := startGRPC(cfg, logger, availabilitySvc, reservationSvc)

	// Protected Backoffice surface (RFC-0023 slice A): the service's first
	// HTTP business routes, verified in-service against the realm (RFC-0022)
	// and role-gated — the edge's JWT check is coarse, this one is
	// authoritative (ADR-047).
	verifier, err := authmw.NewVerifier(authmw.Config{
		Issuer:   cfg.OIDCIssuer,
		Audience: cfg.OIDCAudience,
		JWKSURL:  cfg.OIDCJWKSURL,
	})
	if err != nil {
		logger.Fatal("JWKS verifier init failed", zap.Error(err))
	}
	adminHandler := webv1.NewHandler(logicv1.NewAdminService(
		repository.NewAdminReadRepository(pool),
		repository.NewStockCommandRepository(pool),
	))

	var isShuttingDown atomic.Bool
	srv := setupServer(cfg, &isShuttingDown, pool, adminHandler, verifier)
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
func runSubcommand(cmd string, cfg *config.Config, logger *zap.Logger) bool {
	switch cmd {
	case "migrate":
		if err := migratex.Run(migrations.FS, "sql", cfg.Database.BuildDSN()); err != nil {
			logger.Fatal("Schema migration failed", zap.Error(err))
		}
		logger.Info("Schema migrations applied")
		return true
	case "seed":
		// Demo data is DEV-ONLY; only an explicitly-development environment
		// may seed — staging and anything unrecognised are refused too, not
		// just production.
		if !cfg.IsDevelopment() {
			logger.Fatal("seed refused — demo data is dev-only (ENV must be development)")
		}
		if err := applySeed(cfg); err != nil {
			logger.Fatal("Demo seed failed", zap.Error(err))
		}
		logger.Info("Demo seed data applied")
		return true
	case "backfill":
		// RETIRED in RFC-0021 phase 4 (see the note on runSubcommand). Kept as an
		// explicit refusal, NOT deleted: `default` falls through to serving the app,
		// so without this arm `inventory backfill --apply` would start a full HTTP +
		// gRPC server inside a one-shot Job -- holding product-database credentials,
		// with --apply silently discarded as an unparsed argument, and nothing to
		// reap it. A removed subcommand has to say it was removed; anything else
		// turns a stale runbook line into a running server.
		logger.Fatal("`backfill` was retired in RFC-0021 phase 4 — its source column " +
			"products.stock_quantity no longer exists, and the cross-service grant is " +
			"revoked. Recover a balance at inventory: `seed` (dev only) or an explicit " +
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
	logger *zap.Logger,
	availability *logicv1.AvailabilityService,
	reservations *logicv1.ReservationService,
) (*grpc.Server, *health.Server) {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", ":"+cfg.GRPC.Port)
	if err != nil {
		logger.Fatal("Failed to listen for gRPC", zap.String("port", cfg.GRPC.Port), zap.Error(err))
	}

	grpcSrv, healthSrv := grpcx.NewServer(logger)
	inventoryv1.RegisterInventoryServiceServer(grpcSrv, grpcv1.NewServer(availability, reservations, logger))

	go func() {
		logger.Info("Starting gRPC server", zap.String("port", cfg.GRPC.Port))
		// Serve returns nil after Stop/GracefulStop, so Fatal only fires on a
		// real serve failure.
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Fatal("gRPC server error", zap.Error(err))
		}
	}()

	return grpcSrv, healthSrv
}

func setupServer(cfg *config.Config, isShuttingDown *atomic.Bool, pool interface {
	Ping(context.Context) error
}, adminHandler *webv1.Handler, verifier *authmw.Verifier) *http.Server {
	// Gin defaults to debug mode; anything but development runs release mode
	// so per-route debug banners stay out of production logs.
	if !cfg.IsDevelopment() {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()

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
	logger *zap.Logger,
	isShuttingDown *atomic.Bool,
) {
	go func() {
		logger.Info("Starting inventory service", zap.String("port", cfg.Service.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Failed to start server", zap.Error(err))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	<-ctx.Done()
	logger.Info("Shutdown signal received")

	isShuttingDown.Store(true)
	// Flip the gRPC health status at the start of the drain so clients that
	// watch the health service stop picking this instance while in-flight
	// RPCs finish.
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	drainDelay := cfg.GetReadinessDrainDelayDuration()
	if drainDelay > 0 {
		logger.Info("Readiness drain delay started", zap.Duration("delay", drainDelay))
		time.Sleep(drainDelay)
	}

	shutdownTimeout := cfg.GetShutdownTimeoutDuration()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	logger.Info("Shutting down server...", zap.Duration("timeout", shutdownTimeout))

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	} else {
		logger.Info("HTTP server shutdown complete")
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
		logger.Info("gRPC server shutdown complete")
	case <-time.After(10 * time.Second):
		grpcSrv.Stop()
		<-stopped
		logger.Warn("gRPC server force-stopped after graceful timeout")
	}

	pool.Close()
	logger.Info("Database pool closed")

	// Shutdown the OTel SDK — flushes pending spans plus any OTLP
	// metrics/logs providers built behind the RFC-0014 flags.
	if tp != nil {
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Error("OpenTelemetry shutdown error", zap.Error(err))
		} else {
			logger.Info("OpenTelemetry shutdown complete")
		}
	}

	logger.Info("Graceful shutdown complete")
}
