package config

import (
	"net/url"
	"strings"
	"testing"
)

func TestBuildDSN(t *testing.T) {
	db := &DatabaseConfig{
		Host: "localhost", Port: "5432", Name: "inventory",
		User: "inventory", Password: "secret", SSLMode: "disable",
	}
	want := "postgresql://inventory:secret@localhost:5432/inventory?sslmode=disable"
	if got := db.BuildDSN(); got != want {
		t.Errorf("BuildDSN() = %q, want %q", got, want)
	}
}

func TestBuildDSN_EscapesCredentials(t *testing.T) {
	// Dynamic/rotated secrets can contain DSN-reserved characters; they must
	// round-trip through the URL instead of corrupting it.
	db := &DatabaseConfig{
		Host: "localhost", Port: "5432", Name: "inventory",
		User: "inv@user", Password: "p@ss:w/rd?&", SSLMode: "require",
	}
	u, err := url.Parse(db.BuildDSN())
	if err != nil {
		t.Fatalf("BuildDSN() is not a parseable URL: %v", err)
	}
	if got := u.User.Username(); got != db.User {
		t.Errorf("username round-trip = %q, want %q", got, db.User)
	}
	if got, _ := u.User.Password(); got != db.Password {
		t.Errorf("password round-trip = %q, want %q", got, db.Password)
	}
	if got := u.Query().Get("sslmode"); got != "require" {
		t.Errorf("sslmode = %q, want require", got)
	}
}

func TestProductDBDSN_ExplicitDSNWins(t *testing.T) {
	t.Setenv("PRODUCT_DB_DSN", "postgresql://p:s@phost:5432/product?sslmode=require")
	t.Setenv("PRODUCT_DB_HOST", "ignored")
	got, err := ProductDBDSN()
	if err != nil {
		t.Fatalf("ProductDBDSN() error: %v", err)
	}
	if got != "postgresql://p:s@phost:5432/product?sslmode=require" {
		t.Errorf("ProductDBDSN() = %q, want the explicit DSN", got)
	}
}

func TestProductDBDSN_AssembledFromParts(t *testing.T) {
	t.Setenv("PRODUCT_DB_DSN", "")
	t.Setenv("PRODUCT_DB_HOST", "product-db")
	t.Setenv("PRODUCT_DB_PORT", "")     // default 5432
	t.Setenv("PRODUCT_DB_NAME", "product")
	t.Setenv("PRODUCT_DB_USER", "inv@ro")
	t.Setenv("PRODUCT_DB_PASSWORD", "p@ss/rd?&")
	t.Setenv("PRODUCT_DB_SSLMODE", "") // default require (cross-tenant creds)
	dsn, err := ProductDBDSN()
	if err != nil {
		t.Fatalf("ProductDBDSN() error: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("ProductDBDSN() not parseable: %v", err)
	}
	if u.Host != "product-db:5432" {
		t.Errorf("host = %q, want product-db:5432", u.Host)
	}
	if got, _ := u.User.Password(); got != "p@ss/rd?&" {
		t.Errorf("password round-trip = %q", got)
	}
	if u.Query().Get("sslmode") != "require" {
		t.Errorf("sslmode = %q, want require (secure default)", u.Query().Get("sslmode"))
	}
}

func TestProductDBDSN_SSLModeOptOut(t *testing.T) {
	t.Setenv("PRODUCT_DB_DSN", "")
	t.Setenv("PRODUCT_DB_HOST", "product-db")
	t.Setenv("PRODUCT_DB_NAME", "product")
	t.Setenv("PRODUCT_DB_SSLMODE", "disable") // local dev opts out explicitly
	dsn, err := ProductDBDSN()
	if err != nil {
		t.Fatalf("ProductDBDSN() error: %v", err)
	}
	u, _ := url.Parse(dsn)
	if u.Query().Get("sslmode") != "disable" {
		t.Errorf("sslmode = %q, want disable (explicit opt-out)", u.Query().Get("sslmode"))
	}
}

func TestProductDBDSN_MissingConfigErrors(t *testing.T) {
	for _, k := range []string{"PRODUCT_DB_DSN", "PRODUCT_DB_HOST", "PRODUCT_DB_NAME"} {
		t.Setenv(k, "")
	}
	if _, err := ProductDBDSN(); err == nil {
		t.Fatal("ProductDBDSN() with no config must error")
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Empty value makes getEnv* fall back to the default.
	for _, k := range []string{"SERVICE_NAME", "PORT", "GRPC_PORT", "ENV", "DB_HOST", "DB_POOL_MAX_CONNECTIONS"} {
		t.Setenv(k, "")
	}
	cfg := Load()
	if cfg.Service.Port != "8080" {
		t.Errorf("default Port = %q, want 8080", cfg.Service.Port)
	}
	if cfg.GRPC.Port != "9090" {
		t.Errorf("default GRPC.Port = %q, want 9090", cfg.GRPC.Port)
	}
	if cfg.Database.MaxConnections != 25 {
		t.Errorf("default MaxConnections = %d, want 25", cfg.Database.MaxConnections)
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("SERVICE_NAME", "inventory")
	t.Setenv("PORT", "9999")
	t.Setenv("GRPC_PORT", "9091")
	t.Setenv("ENV", "production")
	t.Setenv("DB_POOL_MAX_CONNECTIONS", "not-a-number") // invalid → falls back to default

	cfg := Load()
	if cfg.Service.Name != "inventory" || cfg.Service.Port != "9999" || cfg.Service.Env != "production" {
		t.Errorf("overrides not applied: %+v", cfg.Service)
	}
	if cfg.GRPC.Port != "9091" {
		t.Errorf("GRPC_PORT override not applied: %q", cfg.GRPC.Port)
	}
	if cfg.Database.MaxConnections != 25 {
		t.Errorf("invalid int env should fall back to 25, got %d", cfg.Database.MaxConnections)
	}
}

// validConfig returns a Config that passes Validate().
func validConfig() *Config {
	c := &Config{}
	c.Service = ServiceConfig{Name: "inventory", Port: "8080", Env: "production"}
	c.GRPC = GRPCConfig{Port: "9090"}
	c.Profiling = ProfilingConfig{Enabled: true, Endpoint: "pyro:4040", ServiceName: "inventory"}
	c.Logging = LoggingConfig{Level: "info", Format: "json"}
	c.Database = DatabaseConfig{} // Host empty → database validation skipped
	return c
}

func TestValidate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("validConfig().Validate() = %v, want nil", err)
	}

	tests := []struct {
		name string
		mut  func(*Config)
	}{
		{"missing service name", func(c *Config) { c.Service.Name = "" }},
		{"non-numeric port", func(c *Config) { c.Service.Port = "abc" }},
		{"missing grpc port", func(c *Config) { c.GRPC.Port = "" }},
		{"non-numeric grpc port", func(c *Config) { c.GRPC.Port = "abc" }},
		{"invalid env", func(c *Config) { c.Service.Env = "qa" }},
		{"profiling endpoint missing", func(c *Config) { c.Profiling.Endpoint = "" }},
		{"invalid log level", func(c *Config) { c.Logging.Level = "trace" }},
		{"invalid log format", func(c *Config) { c.Logging.Format = "xml" }},
		{"db host set but name missing", func(c *Config) { c.Database.Host = "h"; c.Database.User = "u"; c.Database.Password = "p" }},
		{"db bad port", func(c *Config) {
			c.Database.Host = "h"
			c.Database.Name = "n"
			c.Database.User = "u"
			c.Database.Password = "p"
			c.Database.Port = "x"
			c.Database.SSLMode = "disable"
		}},
		{"db invalid sslmode", func(c *Config) {
			c.Database.Host = "h"
			c.Database.Name = "n"
			c.Database.User = "u"
			c.Database.Password = "p"
			c.Database.Port = "5432"
			c.Database.SSLMode = "allow-anything"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mut(c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %q", tt.name)
			}
		})
	}

	t.Run("every allowlisted sslmode passes", func(t *testing.T) {
		for _, mode := range []string{"disable", "prefer", "require", "verify-ca", "verify-full"} {
			c := validConfig()
			c.Database = DatabaseConfig{Host: "h", Port: "5432", Name: "n", User: "u", Password: "p", SSLMode: mode}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() with sslmode=%s = %v, want nil", mode, err)
			}
		}
	})
}

func TestIsDevelopmentProduction(t *testing.T) {
	tests := []struct {
		env    string
		isDev  bool
		isProd bool
	}{
		{"development", true, false},
		{"dev", true, false},
		{"production", false, true},
		{"prod", false, true},
		{"staging", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			c := &Config{}
			c.Service.Env = tt.env
			if c.IsDevelopment() != tt.isDev {
				t.Errorf("IsDevelopment(%q) = %v, want %v", tt.env, c.IsDevelopment(), tt.isDev)
			}
			if c.IsProduction() != tt.isProd {
				t.Errorf("IsProduction(%q) = %v, want %v", tt.env, c.IsProduction(), tt.isProd)
			}
		})
	}
}

func TestGetEnvHelpers(t *testing.T) {
	t.Run("getEnv falls back when empty", func(t *testing.T) {
		t.Setenv("X_TEST_KEY", "")
		if got := getEnv("X_TEST_KEY", "def"); got != "def" {
			t.Errorf("getEnv empty = %q, want def", got)
		}
		t.Setenv("X_TEST_KEY", "set")
		if got := getEnv("X_TEST_KEY", "def"); got != "set" {
			t.Errorf("getEnv set = %q, want set", got)
		}
	})

	t.Run("getEnvBool accepts truthy variants", func(t *testing.T) {
		for _, v := range []string{"true", "1", "yes", "TRUE"} {
			t.Setenv("X_BOOL", v)
			if !getEnvBool("X_BOOL", false) {
				t.Errorf("getEnvBool(%q) = false, want true", v)
			}
		}
		t.Setenv("X_BOOL", "no")
		if getEnvBool("X_BOOL", true) {
			t.Error("getEnvBool(no) = true, want false")
		}
	})

	t.Run("getEnvInt falls back on invalid", func(t *testing.T) {
		t.Setenv("X_INT", "notnum")
		if got := getEnvInt("X_INT", 7); got != 7 {
			t.Errorf("getEnvInt(invalid) = %d, want 7", got)
		}
		t.Setenv("X_INT", "42")
		if got := getEnvInt("X_INT", 7); got != 42 {
			t.Errorf("getEnvInt(42) = %d, want 42", got)
		}
	})
}

func TestValidateErrorMentionsField(t *testing.T) {
	c := validConfig()
	c.Service.Name = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SERVICE_NAME") {
		t.Errorf("expected error mentioning SERVICE_NAME, got %v", err)
	}
}
