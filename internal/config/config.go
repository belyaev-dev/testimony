package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	EnvServerHost         = "TESTIMONY_SERVER_HOST"
	EnvServerPort         = "TESTIMONY_SERVER_PORT"
	EnvServerReadTimeout  = "TESTIMONY_SERVER_READ_TIMEOUT"
	EnvServerWriteTimeout = "TESTIMONY_SERVER_WRITE_TIMEOUT"
	EnvServerIdleTimeout  = "TESTIMONY_SERVER_IDLE_TIMEOUT"
	EnvLogLevel           = "TESTIMONY_LOG_LEVEL"

	EnvS3Endpoint        = "TESTIMONY_S3_ENDPOINT"
	EnvS3Region          = "TESTIMONY_S3_REGION"
	EnvS3Bucket          = "TESTIMONY_S3_BUCKET"
	EnvS3AccessKeyID     = "TESTIMONY_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "TESTIMONY_S3_SECRET_ACCESS_KEY"
	EnvS3UsePathStyle    = "TESTIMONY_S3_USE_PATH_STYLE"

	EnvSQLitePath        = "TESTIMONY_SQLITE_PATH"
	EnvSQLiteBusyTimeout = "TESTIMONY_SQLITE_BUSY_TIMEOUT"

	EnvAuthEnabled       = "TESTIMONY_AUTH_ENABLED"
	EnvAuthAPIKey        = "TESTIMONY_AUTH_API_KEY"
	EnvAuthRequireViewer = "TESTIMONY_AUTH_REQUIRE_VIEWER"

	EnvRetentionDays            = "TESTIMONY_RETENTION_DAYS"
	EnvRetentionCleanupInterval = "TESTIMONY_RETENTION_CLEANUP_INTERVAL"

	EnvGenerateVariant        = "TESTIMONY_GENERATE_VARIANT"
	EnvGenerateCLIPath        = "TESTIMONY_GENERATE_CLI_PATH"
	EnvGenerateTimeout        = "TESTIMONY_GENERATE_TIMEOUT"
	EnvGenerateMaxConcurrency = "TESTIMONY_GENERATE_MAX_CONCURRENCY"
	EnvGenerateHistoryDepth   = "TESTIMONY_GENERATE_HISTORY_DEPTH"

	EnvTempDir            = "TESTIMONY_TEMP_DIR"
	EnvShutdownDrainDelay = "TESTIMONY_SHUTDOWN_DRAIN_DELAY"
	EnvShutdownTimeout    = "TESTIMONY_SHUTDOWN_TIMEOUT"
)

const (
	defaultServerHost         = "0.0.0.0"
	defaultServerPort         = 8080
	defaultServerReadTimeout  = 15 * time.Second
	defaultServerWriteTimeout = 30 * time.Second
	defaultServerIdleTimeout  = 60 * time.Second
	defaultLogLevel           = "INFO"

	defaultS3Endpoint        = "http://127.0.0.1:9000"
	defaultS3Region          = "us-east-1"
	defaultS3Bucket          = "testimony"
	defaultS3AccessKeyID     = "minioadmin"
	defaultS3SecretAccessKey = "minioadmin"
	defaultS3UsePathStyle    = true

	defaultSQLitePath        = "./data/testimony.sqlite"
	defaultSQLiteBusyTimeout = 5 * time.Second

	defaultAuthEnabled       = false
	defaultAuthAPIKey        = ""
	defaultAuthRequireViewer = false

	defaultRetentionDays            = 0
	defaultRetentionCleanupInterval = 1 * time.Hour

	defaultGenerateVariant        = GenerateVariantAllure2
	defaultGenerateCLIPath        = "allure"
	defaultGenerateTimeout        = 2 * time.Minute
	defaultGenerateMaxConcurrency = 2
	defaultGenerateHistoryDepth   = 5

	defaultShutdownDrainDelay = 5 * time.Second
	defaultShutdownTimeout    = 30 * time.Second
)

type LookupFunc func(key string) (string, bool)

type GenerateVariant string

const (
	GenerateVariantAllure2 GenerateVariant = "allure2"
	GenerateVariantAllure3 GenerateVariant = "allure3"
)

type Config struct {
	Server    ServerConfig
	Storage   S3Config
	Database  SQLiteConfig
	Auth      AuthConfig
	Retention RetentionConfig
	Generate  GenerateConfig
	Runtime   RuntimeConfig
	Shutdown  ShutdownConfig
	LogLevel  string
}

type ServerConfig struct {
	Host         string
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

func (c ServerConfig) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool
}

type SQLiteConfig struct {
	Path        string
	BusyTimeout time.Duration
}

type AuthConfig struct {
	Enabled       bool
	APIKey        string
	RequireViewer bool
}

type RetentionConfig struct {
	Days            int
	CleanupInterval time.Duration
}

type GenerateConfig struct {
	Variant        GenerateVariant
	CLIPath        string
	Timeout        time.Duration
	MaxConcurrency int
	HistoryDepth   int
}

type RuntimeConfig struct {
	TempDir string
}

type ShutdownConfig struct {
	ReadyzDrainDelay time.Duration
	Timeout          time.Duration
}

func Load() (Config, error) {
	return LoadFromLookup(os.LookupEnv)
}

func LoadFromLookup(lookup LookupFunc) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	cfg := Config{
		Server: ServerConfig{
			Host:         getString(lookup, EnvServerHost, defaultServerHost),
			ReadTimeout:  defaultServerReadTimeout,
			WriteTimeout: defaultServerWriteTimeout,
			IdleTimeout:  defaultServerIdleTimeout,
		},
		Storage: S3Config{
			Endpoint:        getString(lookup, EnvS3Endpoint, defaultS3Endpoint),
			Region:          getString(lookup, EnvS3Region, defaultS3Region),
			Bucket:          getString(lookup, EnvS3Bucket, defaultS3Bucket),
			AccessKeyID:     getString(lookup, EnvS3AccessKeyID, defaultS3AccessKeyID),
			SecretAccessKey: getString(lookup, EnvS3SecretAccessKey, defaultS3SecretAccessKey),
			UsePathStyle:    defaultS3UsePathStyle,
		},
		Database: SQLiteConfig{
			Path:        getString(lookup, EnvSQLitePath, defaultSQLitePath),
			BusyTimeout: defaultSQLiteBusyTimeout,
		},
		Auth: AuthConfig{
			Enabled:       defaultAuthEnabled,
			APIKey:        getString(lookup, EnvAuthAPIKey, defaultAuthAPIKey),
			RequireViewer: defaultAuthRequireViewer,
		},
		Retention: RetentionConfig{
			Days:            defaultRetentionDays,
			CleanupInterval: defaultRetentionCleanupInterval,
		},
		Generate: GenerateConfig{
			Variant:        defaultGenerateVariant,
			CLIPath:        getString(lookup, EnvGenerateCLIPath, defaultGenerateCLIPath),
			Timeout:        defaultGenerateTimeout,
			MaxConcurrency: defaultGenerateMaxConcurrency,
			HistoryDepth:   defaultGenerateHistoryDepth,
		},
		Runtime: RuntimeConfig{
			TempDir: defaultTempDir(),
		},
		Shutdown: ShutdownConfig{
			ReadyzDrainDelay: defaultShutdownDrainDelay,
			Timeout:          defaultShutdownTimeout,
		},
		LogLevel: getString(lookup, EnvLogLevel, defaultLogLevel),
	}

	var err error
	if cfg.Server.Port, err = getInt(lookup, EnvServerPort, defaultServerPort); err != nil {
		return Config{}, err
	}
	if cfg.Server.ReadTimeout, err = getDuration(lookup, EnvServerReadTimeout, defaultServerReadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Server.WriteTimeout, err = getDuration(lookup, EnvServerWriteTimeout, defaultServerWriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Server.IdleTimeout, err = getDuration(lookup, EnvServerIdleTimeout, defaultServerIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Storage.UsePathStyle, err = getBool(lookup, EnvS3UsePathStyle, defaultS3UsePathStyle); err != nil {
		return Config{}, err
	}
	if cfg.Database.BusyTimeout, err = getDuration(lookup, EnvSQLiteBusyTimeout, defaultSQLiteBusyTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Auth.Enabled, err = getBool(lookup, EnvAuthEnabled, defaultAuthEnabled); err != nil {
		return Config{}, err
	}
	if cfg.Auth.RequireViewer, err = getBool(lookup, EnvAuthRequireViewer, defaultAuthRequireViewer); err != nil {
		return Config{}, err
	}
	if cfg.Retention.Days, err = getInt(lookup, EnvRetentionDays, defaultRetentionDays); err != nil {
		return Config{}, err
	}
	if cfg.Retention.CleanupInterval, err = getDuration(lookup, EnvRetentionCleanupInterval, defaultRetentionCleanupInterval); err != nil {
		return Config{}, err
	}
	if cfg.Generate.Timeout, err = getDuration(lookup, EnvGenerateTimeout, defaultGenerateTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Generate.MaxConcurrency, err = getInt(lookup, EnvGenerateMaxConcurrency, defaultGenerateMaxConcurrency); err != nil {
		return Config{}, err
	}
	if cfg.Generate.HistoryDepth, err = getInt(lookup, EnvGenerateHistoryDepth, defaultGenerateHistoryDepth); err != nil {
		return Config{}, err
	}
	cfg.Generate.Variant = GenerateVariant(getString(lookup, EnvGenerateVariant, string(defaultGenerateVariant)))
	if cfg.Runtime.TempDir, err = getTempDir(lookup, EnvTempDir, defaultTempDir()); err != nil {
		return Config{}, err
	}
	if cfg.Shutdown.ReadyzDrainDelay, err = getDuration(lookup, EnvShutdownDrainDelay, defaultShutdownDrainDelay); err != nil {
		return Config{}, err
	}
	if cfg.Shutdown.Timeout, err = getDuration(lookup, EnvShutdownTimeout, defaultShutdownTimeout); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Host) == "" {
		return fmt.Errorf("%s must not be empty", EnvServerHost)
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535", EnvServerPort)
	}
	if c.Server.ReadTimeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvServerReadTimeout)
	}
	if c.Server.WriteTimeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvServerWriteTimeout)
	}
	if c.Server.IdleTimeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvServerIdleTimeout)
	}
	if strings.TrimSpace(c.LogLevel) == "" {
		return fmt.Errorf("%s must not be empty", EnvLogLevel)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return fmt.Errorf("%s must be a valid slog level: %w", EnvLogLevel, err)
	}
	if strings.TrimSpace(c.Storage.Endpoint) == "" {
		return fmt.Errorf("%s must not be empty", EnvS3Endpoint)
	}
	if strings.TrimSpace(c.Storage.Region) == "" {
		return fmt.Errorf("%s must not be empty", EnvS3Region)
	}
	if strings.TrimSpace(c.Storage.Bucket) == "" {
		return fmt.Errorf("%s must not be empty", EnvS3Bucket)
	}
	if strings.TrimSpace(c.Storage.AccessKeyID) == "" {
		return fmt.Errorf("%s must not be empty", EnvS3AccessKeyID)
	}
	if strings.TrimSpace(c.Storage.SecretAccessKey) == "" {
		return fmt.Errorf("%s must not be empty", EnvS3SecretAccessKey)
	}
	if strings.TrimSpace(c.Database.Path) == "" {
		return fmt.Errorf("%s must not be empty", EnvSQLitePath)
	}
	if c.Database.BusyTimeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvSQLiteBusyTimeout)
	}
	if c.Auth.RequireViewer && !c.Auth.Enabled {
		return fmt.Errorf("%s requires %s to be true", EnvAuthRequireViewer, EnvAuthEnabled)
	}
	if c.Auth.Enabled && strings.TrimSpace(c.Auth.APIKey) == "" {
		return fmt.Errorf("%s must not be empty when %s is true", EnvAuthAPIKey, EnvAuthEnabled)
	}
	if c.Retention.Days < 0 {
		return fmt.Errorf("%s must be zero or greater", EnvRetentionDays)
	}
	if c.Retention.CleanupInterval <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvRetentionCleanupInterval)
	}
	if c.Generate.Variant != GenerateVariantAllure2 && c.Generate.Variant != GenerateVariantAllure3 {
		return fmt.Errorf("%s must be one of %q or %q", EnvGenerateVariant, GenerateVariantAllure2, GenerateVariantAllure3)
	}
	if strings.TrimSpace(c.Generate.CLIPath) == "" {
		return fmt.Errorf("%s must not be empty", EnvGenerateCLIPath)
	}
	if c.Generate.Timeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvGenerateTimeout)
	}
	if c.Generate.MaxConcurrency <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvGenerateMaxConcurrency)
	}
	if c.Generate.HistoryDepth < 0 {
		return fmt.Errorf("%s must be zero or greater", EnvGenerateHistoryDepth)
	}
	if strings.TrimSpace(c.Runtime.TempDir) == "" {
		return fmt.Errorf("%s must not be empty", EnvTempDir)
	}
	if c.Shutdown.ReadyzDrainDelay < 0 {
		return fmt.Errorf("%s must be zero or greater", EnvShutdownDrainDelay)
	}
	if c.Shutdown.Timeout <= 0 {
		return fmt.Errorf("%s must be greater than zero", EnvShutdownTimeout)
	}

	return nil
}

func defaultTempDir() string {
	return filepath.Join(os.TempDir(), "testimony")
}

func getString(lookup LookupFunc, key, fallback string) string {
	if value, ok := lookup(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func getInt(lookup LookupFunc, key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}

	return parsed, nil
}

func getBool(lookup LookupFunc, key string, fallback bool) (bool, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}

	return parsed, nil
}

func getDuration(lookup LookupFunc, key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}

	return parsed, nil
}

func getTempDir(lookup LookupFunc, key, fallback string) (string, error) {
	value, ok := lookup(key)
	if !ok {
		return filepath.Clean(fallback), nil
	}

	cleaned := filepath.Clean(strings.TrimSpace(value))
	if cleaned == "." {
		return "", fmt.Errorf("%s must not resolve to the current directory", key)
	}

	return cleaned, nil
}
