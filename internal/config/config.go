// Package config loads service configuration from .env files and the process environment.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Env is the runtime environment the service runs in.
type Env string

const (
	EnvDebug   Env = "DEBUG"
	EnvTesting Env = "TESTING"
	EnvProd    Env = "PROD"
)

// Config holds all runtime settings.
type Config struct {
	Env             Env
	HTTPAddr        string
	DBPath          string
	LogFile         string
	ShutdownTimeout time.Duration
	RequestTimeout  time.Duration
	DedupTTL        time.Duration

	// Default max_parallel_chunks resolution (see settings.GetDefaultMaxParallelChunks).
	SettingsFile            string // HIVE_SETTINGS_FILE
	HiveAPI                 string // HIVE_API
	AccountMaxChunksDefault int    // ACCOUNT_MAX_CHUNKS_DEFAULT

	// RateLimit is the number of requests per second allowed per client IP
	// (RATE_LIMIT, 0 = disabled).
	RateLimit int
	// TrustedProxies are CIDRs whose X-Forwarded-For is trusted for the
	// client IP (TRUSTED_PROXIES, comma-separated). Empty = use the TCP peer.
	TrustedProxies []string
}

// IsProd reports whether the service runs in production mode.
func (c Config) IsProd() bool { return c.Env == EnvProd }

// Load reads .env (does not override variables already set in the process
// environment), then .env.override (which overrides everything), then
// applies defaults for anything still unset.
func Load() (Config, error) {
	if err := godotenv.Load(".env"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	if err := godotenv.Overload(".env.override"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env.override: %w", err)
	}

	cfg := Config{
		Env:      Env(strings.ToUpper(get("ENV", string(EnvDebug)))),
		HTTPAddr: get("HTTP_ADDR", ":8080"),
		DBPath:   get("DB_PATH", "data/hive.db"),
		LogFile:  get("LOG_FILE", "logs/app.log"),

		SettingsFile: get("HIVE_SETTINGS_FILE", "hivesettings.json"),
		HiveAPI:      get("HIVE_API", ""),
	}

	var err error
	if cfg.ShutdownTimeout, err = time.ParseDuration(get("SHUTDOWN_TIMEOUT", "15s")); err != nil {
		return Config{}, fmt.Errorf("invalid SHUTDOWN_TIMEOUT: %w", err)
	}
	if cfg.RequestTimeout, err = time.ParseDuration(get("REQUEST_TIMEOUT", "30s")); err != nil {
		return Config{}, fmt.Errorf("invalid REQUEST_TIMEOUT: %w", err)
	}
	if cfg.DedupTTL, err = time.ParseDuration(get("DEDUP_TTL", "24h")); err != nil {
		return Config{}, fmt.Errorf("invalid DEDUP_TTL: %w", err)
	}
	if cfg.AccountMaxChunksDefault, err = strconv.Atoi(get("ACCOUNT_MAX_CHUNKS_DEFAULT", "5")); err != nil || cfg.AccountMaxChunksDefault <= 0 {
		return Config{}, fmt.Errorf("invalid ACCOUNT_MAX_CHUNKS_DEFAULT: must be a positive integer")
	}
	if cfg.RateLimit, err = strconv.Atoi(get("RATE_LIMIT", "10")); err != nil || cfg.RateLimit < 0 {
		return Config{}, fmt.Errorf("invalid RATE_LIMIT: must be a non-negative integer (requests per second)")
	}
	for _, p := range strings.Split(get("TRUSTED_PROXIES", ""), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if _, err := netip.ParsePrefix(p); err != nil {
			return Config{}, fmt.Errorf("invalid TRUSTED_PROXIES entry %q: %w", p, err)
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, p)
	}

	switch cfg.Env {
	case EnvDebug, EnvTesting, EnvProd:
	default:
		return Config{}, fmt.Errorf("invalid ENV %q (expected DEBUG, TESTING or PROD)", cfg.Env)
	}
	return cfg, nil
}

func get(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}
