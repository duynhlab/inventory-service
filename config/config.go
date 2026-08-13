// Package config provides centralized configuration management for all microservices
// with validation, type safety, and clear documentation for SRE/DevOps teams.
//
// Configuration Sources (12-factor app principles):
//  1. Default values (hardcoded)
//  2. .env file (local development via godotenv)
//  3. Environment variables (Kubernetes runtime)
//  4. Helm values → deployment.yaml → env/extraEnv → container environment
//
// Usage:
//
//	import "github.com/duynhlab/inventory-service/config"
//
//	func main() {
//	    cfg := config.Load()
//	    if err := cfg.Validate(); err != nil {
//	        log.Fatal(err)
//	    }
//	    // Use cfg.Service.Port, cfg.Database.Host, etc.
//	}
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// defaultServiceName is the fallback service name when SERVICE_NAME is not set
const defaultServiceName = "unknown"

// Config holds all configuration for a microservice. OpenTelemetry wiring
// (traces/metrics/logs) is configured by pkg/obsx directly from the
// environment, so there is no Tracing section here.
type Config struct {
	Service         ServiceConfig   // Service-specific settings (port, name, version)
	GRPC            GRPCConfig      // Internal gRPC server
	Profiling       ProfilingConfig // Pyroscope continuous profiling
	Logging         LoggingConfig   // Structured logging (Zap)
	Database        DatabaseConfig  // PostgreSQL database configuration
	ShutdownTimeout int             // Graceful shutdown timeout in seconds - from SHUTDOWN_TIMEOUT env (default: 10)

	// OIDC verification for the protected web surface (RFC-0023): the realm
	// issuer the fleet trusts (RFC-0022/0024). Mirrors payment-service's env
	// contract exactly — OIDC_ISSUER / OIDC_AUDIENCE / OIDC_JWKS_URL.
	OIDCIssuer   string // Expected OIDC issuer (iss, exact match) - from OIDC_ISSUER env
	OIDCAudience string // Expected OIDC audience (aud containment) - from OIDC_AUDIENCE env
	OIDCJWKSURL  string // Optional JWKS endpoint override - from OIDC_JWKS_URL env (empty = derived from issuer)
	// ReadinessDrainDelay: delay after failing readiness before shutting down the HTTP server.
	// This gives Kubernetes/Service routing time to stop sending new traffic.
	// From READINESS_DRAIN_DELAY env (default: 5s, max: 30s).
	ReadinessDrainDelay int
}

// GRPCConfig defines the internal gRPC server (east-west only). gRPC is the
// official east-west transport, so the server always runs; only the port is
// configurable. HTTP :8080 is unaffected.
type GRPCConfig struct {
	Port string // GRPC_PORT (default "9090")
}

// ServiceConfig defines basic service configuration
type ServiceConfig struct {
	Name    string // Service name (e.g., "inventory") - from SERVICE_NAME env
	Port    string // HTTP server port (default: "8080") - from PORT env
	Version string // Service version (optional) - from VERSION env
	Env     string // Environment (dev/staging/production) - from ENV env
}

// ProfilingConfig defines Pyroscope continuous profiling configuration
type ProfilingConfig struct {
	Enabled     bool   // Enable profiling (default: true) - from PROFILING_ENABLED env
	Endpoint    string // Pyroscope endpoint - from PYROSCOPE_ENDPOINT env
	ServiceName string // Service name for profiling (defaults to ServiceConfig.Name)
}

// LoggingConfig defines structured logging configuration
type LoggingConfig struct {
	Level  string // Log level: debug, info, warn, error (default: "info") - from LOG_LEVEL env
	Format string // Log format: json, console (default: "json") - from LOG_FORMAT env
}

// DatabaseConfig defines PostgreSQL database configuration
// All database connections use separate environment variables (not DATABASE_URL string)
type DatabaseConfig struct {
	Host           string // Database host - from DB_HOST env
	Port           string // Database port - from DB_PORT env (default: "5432")
	Name           string // Database name - from DB_NAME env
	User           string // Database user - from DB_USER env
	Password       string // Database password - from DB_PASSWORD env
	SSLMode        string // SSL mode - from DB_SSLMODE env (default: "disable")
	MaxConnections int    // Max connections - from DB_POOL_MAX_CONNECTIONS env (default: 25)
	PoolMode       string // Pool mode - from DB_POOL_MODE env (optional)
	PoolerType     string // Pooler type - from DB_POOLER_TYPE env (optional)
}

// BuildDSN constructs the PostgreSQL connection string from config. It is the
// single source of truth for the DSN: both the `migrate` subcommand and the
// app's connection pool use it, so they connect identically.
func (c *DatabaseConfig) BuildDSN() string {
	// Format: postgresql://user:password@host:port/dbname?sslmode=disable
	// Built via net/url so credentials with reserved characters (rotated or
	// dynamic secrets) are percent-encoded instead of corrupting the DSN.
	// Pool sizing is applied on the parsed pgxpool.Config in database.Connect (not
	// the DSN) so the migrate subcommand can share this exact DSN (its pgx stdlib
	// driver rejects pool_* params).
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(c.User, c.Password),
		Host:     net.JoinHostPort(c.Host, c.Port),
		Path:     "/" + c.Name,
		RawQuery: url.Values{"sslmode": []string{c.SSLMode}}.Encode(),
	}
	return u.String()
}

// Load reads configuration from environment variables with defaults
// It automatically loads .env file if present (for local development)
//
// Priority: .env file < environment variables
// This means ENV vars override .env file values (production takes precedence)
func Load() *Config {
	// Load .env file if exists (for local development)
	// godotenv.Load() fails silently if .env doesn't exist - perfect for production
	_ = godotenv.Load()

	return &Config{
		Service: ServiceConfig{
			Name:    getEnv("SERVICE_NAME", defaultServiceName),
			Port:    getEnv("PORT", "8080"),
			Version: getEnv("VERSION", "dev"),
			Env:     getEnv("ENV", "development"),
		},
		GRPC: GRPCConfig{
			Port: getEnv("GRPC_PORT", "9090"),
		},
		Profiling: ProfilingConfig{
			Enabled:     getEnvBool("PROFILING_ENABLED", true),
			Endpoint:    getEnv("PYROSCOPE_ENDPOINT", "http://pyroscope.monitoring.svc.cluster.local:4040"),
			ServiceName: getEnv("SERVICE_NAME", defaultServiceName),
		},
		Logging: LoggingConfig{
			Level:  getEnv("LOG_LEVEL", "info"),
			Format: getEnv("LOG_FORMAT", "json"),
		},
		Database: DatabaseConfig{
			Host:           getEnv("DB_HOST", ""),
			Port:           getEnv("DB_PORT", "5432"),
			Name:           getEnv("DB_NAME", ""),
			User:           getEnv("DB_USER", ""),
			Password:       getEnv("DB_PASSWORD", ""),
			SSLMode:        getEnv("DB_SSLMODE", "disable"),
			MaxConnections: getEnvInt("DB_POOL_MAX_CONNECTIONS", 25),
			PoolMode:       getEnv("DB_POOL_MODE", ""),
			PoolerType:     getEnv("DB_POOLER_TYPE", ""),
		},
		ShutdownTimeout:     getEnvDurationSeconds("SHUTDOWN_TIMEOUT", 10),
		ReadinessDrainDelay: getEnvDurationSecondsWithMax("READINESS_DRAIN_DELAY", 5, 30),
		OIDCIssuer:          getEnv("OIDC_ISSUER", "https://id.duynh.me/realms/duynhlab"),
		OIDCAudience:        getEnv("OIDC_AUDIENCE", "duynhlab-platform"),
		OIDCJWKSURL:         getEnv("OIDC_JWKS_URL", ""),
	}
}

// Validate performs comprehensive validation of all configuration fields
// Returns detailed error messages for SRE/DevOps troubleshooting
func (c *Config) Validate() error {
	var errs []string

	errs = append(errs, c.validateService()...)
	errs = append(errs, c.validateGRPC()...)
	errs = append(errs, c.validateProfiling()...)
	errs = append(errs, c.validateLogging()...)
	errs = append(errs, c.validateDatabase()...)

	if len(errs) > 0 {
		return fmt.Errorf("configuration validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

// ValidateForSubcommand validates only what a `migrate` or `seed`
// run needs: the database. Those subcommands apply an embedded SQL set and
// exit — they never serve HTTP or gRPC — and the mop chart's init container
// passes only DB_* env, the same contract every other service on the platform
// runs under. Demanding SERVICE_NAME/PORT/GRPC_PORT here crash-loops the init
// container instead of running the migration.
//
// DB_HOST is required rather than optional (Validate treats an unset host as
// "no database configured"): a subcommand with no host would otherwise get past
// validation and fail later inside the driver with a far less obvious message.
func (c *Config) ValidateForSubcommand() error {
	var errs []string

	if c.Database.Host == "" {
		errs = append(errs, "DB_HOST is required (subcommands connect to the database)")
	}
	errs = append(errs, c.validateDatabase()...)

	if len(errs) > 0 {
		return fmt.Errorf("configuration validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

func (c *Config) validateService() []string {
	var errs []string
	if c.Service.Name == "" || c.Service.Name == defaultServiceName {
		errs = append(errs, "SERVICE_NAME is required (e.g., 'inventory')")
	}
	if c.Service.Port == "" {
		errs = append(errs, "PORT is required (e.g., '8080')")
	}
	if _, err := strconv.Atoi(c.Service.Port); err != nil {
		errs = append(errs, "PORT must be a valid number, got: "+c.Service.Port)
	}
	validEnvs := []string{"development", "dev", "staging", "stage", "production", "prod"}
	if !contains(validEnvs, c.Service.Env) {
		errs = append(errs, fmt.Sprintf("ENV must be one of %v, got: %s", validEnvs, c.Service.Env))
	}
	return errs
}

func (c *Config) validateGRPC() []string {
	var errs []string
	if c.GRPC.Port == "" {
		errs = append(errs, "GRPC_PORT is required (e.g., '9090')")
	}
	if _, err := strconv.Atoi(c.GRPC.Port); err != nil {
		errs = append(errs, "GRPC_PORT must be a valid number, got: "+c.GRPC.Port)
	}
	return errs
}

func (c *Config) validateProfiling() []string {
	if !c.Profiling.Enabled {
		return nil
	}
	var errs []string
	if c.Profiling.Endpoint == "" {
		errs = append(errs, "PYROSCOPE_ENDPOINT is required when profiling is enabled")
	}
	if c.Profiling.ServiceName == "" || c.Profiling.ServiceName == defaultServiceName {
		errs = append(errs, "SERVICE_NAME is required for profiling (used in Pyroscope UI)")
	}
	return errs
}

func (c *Config) validateLogging() []string {
	var errs []string
	validLogLevels := []string{"debug", "info", "warn", "error"}
	if !contains(validLogLevels, strings.ToLower(c.Logging.Level)) {
		errs = append(errs, fmt.Sprintf("LOG_LEVEL must be one of %v, got: %s", validLogLevels, c.Logging.Level))
	}
	validLogFormats := []string{"json", "console"}
	if !contains(validLogFormats, strings.ToLower(c.Logging.Format)) {
		errs = append(errs, fmt.Sprintf("LOG_FORMAT must be one of %v, got: %s", validLogFormats, c.Logging.Format))
	}
	return errs
}

func (c *Config) validateDatabase() []string {
	if c.Database.Host == "" {
		return nil
	}
	var errs []string
	if c.Database.Name == "" {
		errs = append(errs, "DB_NAME is required when DB_HOST is set")
	}
	if c.Database.User == "" {
		errs = append(errs, "DB_USER is required when DB_HOST is set")
	}
	if c.Database.Password == "" {
		errs = append(errs, "DB_PASSWORD is required when DB_HOST is set")
	}
	if c.Database.Port != "" {
		if _, err := strconv.Atoi(c.Database.Port); err != nil {
			errs = append(errs, "DB_PORT must be a valid number, got: "+c.Database.Port)
		}
	}
	validSSLModes := []string{"disable", "prefer", "require", "verify-ca", "verify-full"}
	if !contains(validSSLModes, c.Database.SSLMode) {
		errs = append(errs, fmt.Sprintf("DB_SSLMODE must be one of %v, got: %s", validSSLModes, c.Database.SSLMode))
	}
	return errs
}

// IsDevelopment returns true if running in development environment
func (c *Config) IsDevelopment() bool {
	env := strings.ToLower(c.Service.Env)
	return env == "development" || env == "dev"
}

// IsProduction returns true if running in production environment
func (c *Config) IsProduction() bool {
	env := strings.ToLower(c.Service.Env)
	return env == "production" || env == "prod"
}

// Helper functions for environment variable parsing

// getEnv reads an environment variable with a default fallback
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvBool reads a boolean environment variable with a default fallback
// Accepts: "true", "1", "yes" for true | "false", "0", "no" for false
func getEnvBool(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	value = strings.ToLower(value)
	return value == "true" || value == "1" || value == "yes"
}

// getEnvInt reads an integer environment variable with a default fallback
// Returns default if parsing fails
func getEnvInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	intValue, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return intValue
}

// getEnvDurationSeconds reads a duration environment variable and returns seconds as int
// Accepts Go duration format (e.g., "10s", "30s", "1m")
// Default: 10 seconds
// Max: 60 seconds (safety limit)
// Returns default on invalid values (silent fallback for startup safety)
func getEnvDurationSeconds(key string, defaultValueSeconds int) int {
	const maxSeconds = 60

	timeoutStr := os.Getenv(key)
	if timeoutStr == "" {
		return defaultValueSeconds
	}

	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		// Invalid format - use default (silent fallback for startup safety)
		return defaultValueSeconds
	}

	// Convert to seconds
	seconds := int(timeout.Seconds())

	// Validate: must be positive and within reasonable limit
	if seconds <= 0 || seconds > maxSeconds {
		// Invalid value - use default (silent fallback for startup safety)
		return defaultValueSeconds
	}

	return seconds
}

// getEnvDurationSecondsWithMax reads a duration env var and returns seconds as int.
// Accepts Go duration format (e.g., "5s", "30s", "1m").
// Returns default on invalid values (silent fallback for startup safety).
func getEnvDurationSecondsWithMax(key string, defaultValueSeconds int, maxSeconds int) int {
	timeoutStr := os.Getenv(key)
	if timeoutStr == "" {
		return defaultValueSeconds
	}

	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return defaultValueSeconds
	}

	seconds := int(timeout.Seconds())
	if seconds <= 0 || seconds > maxSeconds {
		return defaultValueSeconds
	}

	return seconds
}

// GetShutdownTimeoutDuration returns shutdown timeout as time.Duration
// Convenience method for use in main.go
func (c *Config) GetShutdownTimeoutDuration() time.Duration {
	return time.Duration(c.ShutdownTimeout) * time.Second
}

// GetReadinessDrainDelayDuration returns readiness drain delay as time.Duration.
func (c *Config) GetReadinessDrainDelayDuration() time.Duration {
	return time.Duration(c.ReadinessDrainDelay) * time.Second
}

// contains checks if a string slice contains a specific value
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}
