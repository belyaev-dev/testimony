package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/config"
)

func TestLoadFromLookupDefaults(t *testing.T) {
	cfg, err := config.LoadFromLookup(mapLookup(map[string]string{}))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if got, want := cfg.Server.Host, "0.0.0.0"; got != want {
		t.Fatalf("Server.Host = %q, want %q", got, want)
	}
	if got, want := cfg.Server.Port, 8080; got != want {
		t.Fatalf("Server.Port = %d, want %d", got, want)
	}
	if got, want := cfg.LogLevel, "INFO"; got != want {
		t.Fatalf("LogLevel = %q, want %q", got, want)
	}
	if got, want := cfg.Storage.Endpoint, "http://127.0.0.1:9000"; got != want {
		t.Fatalf("Storage.Endpoint = %q, want %q", got, want)
	}
	if got, want := cfg.Storage.Bucket, "testimony"; got != want {
		t.Fatalf("Storage.Bucket = %q, want %q", got, want)
	}
	if got, want := cfg.Database.Path, "./data/testimony.sqlite"; got != want {
		t.Fatalf("Database.Path = %q, want %q", got, want)
	}
	if got, want := cfg.Auth.Enabled, false; got != want {
		t.Fatalf("Auth.Enabled = %t, want %t", got, want)
	}
	if got, want := cfg.Auth.APIKey, ""; got != want {
		t.Fatalf("Auth.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.Auth.RequireViewer, false; got != want {
		t.Fatalf("Auth.RequireViewer = %t, want %t", got, want)
	}
	if got, want := cfg.Retention.Days, 0; got != want {
		t.Fatalf("Retention.Days = %d, want %d", got, want)
	}
	if got, want := cfg.Retention.CleanupInterval, time.Hour; got != want {
		t.Fatalf("Retention.CleanupInterval = %s, want %s", got, want)
	}
	if got, want := cfg.Generate.Variant, config.GenerateVariantAllure2; got != want {
		t.Fatalf("Generate.Variant = %q, want %q", got, want)
	}
	if got, want := cfg.Generate.CLIPath, "allure"; got != want {
		t.Fatalf("Generate.CLIPath = %q, want %q", got, want)
	}
	if got, want := cfg.Generate.Timeout, 2*time.Minute; got != want {
		t.Fatalf("Generate.Timeout = %s, want %s", got, want)
	}
	if got, want := cfg.Generate.MaxConcurrency, 2; got != want {
		t.Fatalf("Generate.MaxConcurrency = %d, want %d", got, want)
	}
	if got, want := cfg.Generate.HistoryDepth, 5; got != want {
		t.Fatalf("Generate.HistoryDepth = %d, want %d", got, want)
	}
	if got, want := cfg.Runtime.TempDir, filepath.Join(os.TempDir(), "testimony"); got != want {
		t.Fatalf("Runtime.TempDir = %q, want %q", got, want)
	}
	if got, want := cfg.Shutdown.ReadyzDrainDelay, 5*time.Second; got != want {
		t.Fatalf("Shutdown.ReadyzDrainDelay = %s, want %s", got, want)
	}
}

func TestLoadFromLookupOverrides(t *testing.T) {
	env := map[string]string{
		config.EnvServerHost:               "127.0.0.1",
		config.EnvServerPort:               "9090",
		config.EnvServerReadTimeout:        "20s",
		config.EnvServerWriteTimeout:       "45s",
		config.EnvServerIdleTimeout:        "75s",
		config.EnvLogLevel:                 "debug",
		config.EnvS3Endpoint:               "http://minio.internal:9000",
		config.EnvS3Region:                 "eu-central-1",
		config.EnvS3Bucket:                 "reports",
		config.EnvS3AccessKeyID:            "access",
		config.EnvS3SecretAccessKey:        "secret",
		config.EnvS3UsePathStyle:           "false",
		config.EnvSQLitePath:               "/tmp/testimony.sqlite",
		config.EnvSQLiteBusyTimeout:        "9s",
		config.EnvAuthEnabled:              "true",
		config.EnvAuthAPIKey:               "bootstrap-key-test",
		config.EnvAuthRequireViewer:        "true",
		config.EnvRetentionDays:            "14",
		config.EnvRetentionCleanupInterval: "15m",
		config.EnvGenerateVariant:          "allure3",
		config.EnvGenerateCLIPath:          "/opt/allure/bin/allure",
		config.EnvGenerateTimeout:          "3m",
		config.EnvGenerateMaxConcurrency:   "4",
		config.EnvGenerateHistoryDepth:     "9",
		config.EnvTempDir:                  "/tmp/testimony/work",
		config.EnvShutdownDrainDelay:       "2s",
		config.EnvShutdownTimeout:          "40s",
	}

	cfg, err := config.LoadFromLookup(mapLookup(env))
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if got, want := cfg.Server.Address(), "127.0.0.1:9090"; got != want {
		t.Fatalf("Server.Address() = %q, want %q", got, want)
	}
	if got, want := cfg.Server.ReadTimeout, 20*time.Second; got != want {
		t.Fatalf("Server.ReadTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.Server.WriteTimeout, 45*time.Second; got != want {
		t.Fatalf("Server.WriteTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.Server.IdleTimeout, 75*time.Second; got != want {
		t.Fatalf("Server.IdleTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.LogLevel, "debug"; got != want {
		t.Fatalf("LogLevel = %q, want %q", got, want)
	}
	if got, want := cfg.Storage.Region, "eu-central-1"; got != want {
		t.Fatalf("Storage.Region = %q, want %q", got, want)
	}
	if got, want := cfg.Storage.UsePathStyle, false; got != want {
		t.Fatalf("Storage.UsePathStyle = %t, want %t", got, want)
	}
	if got, want := cfg.Database.BusyTimeout, 9*time.Second; got != want {
		t.Fatalf("Database.BusyTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.Auth.Enabled, true; got != want {
		t.Fatalf("Auth.Enabled = %t, want %t", got, want)
	}
	if got, want := cfg.Auth.APIKey, "bootstrap-key-test"; got != want {
		t.Fatalf("Auth.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.Auth.RequireViewer, true; got != want {
		t.Fatalf("Auth.RequireViewer = %t, want %t", got, want)
	}
	if got, want := cfg.Retention.Days, 14; got != want {
		t.Fatalf("Retention.Days = %d, want %d", got, want)
	}
	if got, want := cfg.Retention.CleanupInterval, 15*time.Minute; got != want {
		t.Fatalf("Retention.CleanupInterval = %s, want %s", got, want)
	}
	if got, want := cfg.Generate.Variant, config.GenerateVariantAllure3; got != want {
		t.Fatalf("Generate.Variant = %q, want %q", got, want)
	}
	if got, want := cfg.Generate.CLIPath, "/opt/allure/bin/allure"; got != want {
		t.Fatalf("Generate.CLIPath = %q, want %q", got, want)
	}
	if got, want := cfg.Generate.Timeout, 3*time.Minute; got != want {
		t.Fatalf("Generate.Timeout = %s, want %s", got, want)
	}
	if got, want := cfg.Generate.MaxConcurrency, 4; got != want {
		t.Fatalf("Generate.MaxConcurrency = %d, want %d", got, want)
	}
	if got, want := cfg.Generate.HistoryDepth, 9; got != want {
		t.Fatalf("Generate.HistoryDepth = %d, want %d", got, want)
	}
	if got, want := cfg.Runtime.TempDir, filepath.Clean("/tmp/testimony/work"); got != want {
		t.Fatalf("Runtime.TempDir = %q, want %q", got, want)
	}
	if got, want := cfg.Shutdown.Timeout, 40*time.Second; got != want {
		t.Fatalf("Shutdown.Timeout = %s, want %s", got, want)
	}
}

func TestLoadFromLookupValidation(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "invalid port",
			env: map[string]string{
				config.EnvServerPort: "nope",
			},
		},
		{
			name: "invalid bool",
			env: map[string]string{
				config.EnvS3UsePathStyle: "maybe",
			},
		},
		{
			name: "auth enabled without api key",
			env: map[string]string{
				config.EnvAuthEnabled: "true",
			},
		},
		{
			name: "viewer auth without auth enabled",
			env: map[string]string{
				config.EnvAuthRequireViewer: "true",
				config.EnvAuthAPIKey:        "bootstrap-key-test",
			},
		},
		{
			name: "negative retention days",
			env: map[string]string{
				config.EnvRetentionDays: "-1",
			},
		},
		{
			name: "zero retention cleanup interval",
			env: map[string]string{
				config.EnvRetentionCleanupInterval: "0s",
			},
		},
		{
			name: "empty bucket",
			env: map[string]string{
				config.EnvS3Bucket: "   ",
			},
		},
		{
			name: "invalid generate variant",
			env: map[string]string{
				config.EnvGenerateVariant: "legacy",
			},
		},
		{
			name: "empty generate cli path",
			env: map[string]string{
				config.EnvGenerateCLIPath: "   ",
			},
		},
		{
			name: "zero max concurrency",
			env: map[string]string{
				config.EnvGenerateMaxConcurrency: "0",
			},
		},
		{
			name: "negative history depth",
			env: map[string]string{
				config.EnvGenerateHistoryDepth: "-1",
			},
		},
		{
			name: "negative drain delay",
			env: map[string]string{
				config.EnvShutdownDrainDelay: "-1s",
			},
		},
		{
			name: "invalid temp dir",
			env: map[string]string{
				config.EnvTempDir: " . ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := config.LoadFromLookup(mapLookup(tt.env)); err == nil {
				t.Fatalf("LoadFromLookup() error = nil, want validation error")
			}
		})
	}
}

func mapLookup(values map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
