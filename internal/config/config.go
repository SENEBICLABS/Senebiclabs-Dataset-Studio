// Package config loads and validates process configuration from environment
// variables. It has no package-level state: callers pass a lookup function,
// which keeps tests hermetic.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Lookup matches os.LookupEnv.
type Lookup func(key string) (string, bool)

// Environment variable names.
const (
	EnvLogLevel          = "STUDIO_LOG_LEVEL"
	EnvLogFormat         = "STUDIO_LOG_FORMAT"
	EnvHTTPAddr          = "STUDIO_HTTP_ADDR"
	EnvDatabaseURL       = "STUDIO_DATABASE_URL"
	EnvShutdownTimeout   = "STUDIO_SHUTDOWN_TIMEOUT"
	EnvReadinessTimeout  = "STUDIO_READINESS_TIMEOUT"
	EnvReadHeaderTimeout = "STUDIO_HTTP_READ_HEADER_TIMEOUT"
	EnvReadTimeout       = "STUDIO_HTTP_READ_TIMEOUT"
	EnvWriteTimeout      = "STUDIO_HTTP_WRITE_TIMEOUT"
	EnvIdleTimeout       = "STUDIO_HTTP_IDLE_TIMEOUT"
)

// Config is the full process configuration. Components read only the parts
// they need.
type Config struct {
	LogLevel  string // debug | info | warn | error
	LogFormat string // json | text

	// DatabaseURL is optional in M0. When empty, readiness reports no database
	// check. From M1 each component connects with its own database role.
	DatabaseURL string

	ShutdownTimeout  time.Duration
	ReadinessTimeout time.Duration

	HTTP HTTP
}

// HTTP configures the API listener.
type HTTP struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Defaults returns the configuration used when no variables are set.
func Defaults() Config {
	return Config{
		LogLevel:         "info",
		LogFormat:        "json",
		ShutdownTimeout:  20 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		HTTP: HTTP{
			Addr:              "127.0.0.1:8480",
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
	}
}

// Load reads configuration through lookup and validates it. All problems are
// reported together.
func Load(lookup Lookup) (Config, error) {
	c := Defaults()
	var errs []error

	str := func(key string, dst *string) {
		if v, ok := lookup(key); ok {
			*dst = strings.TrimSpace(v)
		}
	}
	dur := func(key string, dst *time.Duration) {
		v, ok := lookup(key)
		if !ok {
			return
		}
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q", key, v))
			return
		}
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s: must be positive, got %s", key, d))
			return
		}
		*dst = d
	}

	str(EnvLogLevel, &c.LogLevel)
	str(EnvLogFormat, &c.LogFormat)
	str(EnvHTTPAddr, &c.HTTP.Addr)
	str(EnvDatabaseURL, &c.DatabaseURL)
	dur(EnvShutdownTimeout, &c.ShutdownTimeout)
	dur(EnvReadinessTimeout, &c.ReadinessTimeout)
	dur(EnvReadHeaderTimeout, &c.HTTP.ReadHeaderTimeout)
	dur(EnvReadTimeout, &c.HTTP.ReadTimeout)
	dur(EnvWriteTimeout, &c.HTTP.WriteTimeout)
	dur(EnvIdleTimeout, &c.HTTP.IdleTimeout)

	c.LogLevel = strings.ToLower(c.LogLevel)
	c.LogFormat = strings.ToLower(c.LogFormat)

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("%s: must be one of debug, info, warn, error; got %q", EnvLogLevel, c.LogLevel))
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("%s: must be json or text; got %q", EnvLogFormat, c.LogFormat))
	}
	if err := validateAddr(c.HTTP.Addr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvHTTPAddr, err))
	}
	if c.DatabaseURL != "" {
		if err := validateDatabaseURL(c.DatabaseURL); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvDatabaseURL, err))
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func validateAddr(addr string) error {
	// An empty host means all interfaces: allowed, but not the default.
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

// ValidateDatabaseURL checks that raw is a postgres:// or postgresql:// URL
// with a host. Errors never echo the input, which may contain a password.
func ValidateDatabaseURL(raw string) error { return validateDatabaseURL(raw) }

func validateDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// Never echo the raw value: it may contain a password.
		return errors.New("not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("scheme must be postgres or postgresql, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// RedactedDatabaseURL returns the database URL with every secret replaced.
// It is for logs and CLI output only; DatabaseURL itself is what pgx uses.
func (c Config) RedactedDatabaseURL() string {
	return RedactURL(c.DatabaseURL)
}

// redactedValue replaces every secret in redacted output.
const redactedValue = "xxxxx"

// sensitiveQueryParams are connection-URL query parameters that carry
// secrets. PostgreSQL URLs accept the password both in the userinfo part and
// as ?password=; sslpassword unlocks an encrypted client key. Names are
// compared case-insensitively, which can only over-redact.
var sensitiveQueryParams = map[string]bool{
	"password":    true,
	"sslpassword": true,
}

// RedactURL returns a display-only copy of a connection URL with secrets
// replaced: the userinfo password and the values of sensitive query
// parameters. Other query parameters keep their original order and encoding.
// Input that is not a parseable URL with a scheme (for example a key/value
// DSN) is fully redacted rather than echoed. The result must never be used to
// connect.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "[redacted]"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), redactedValue)
	}
	u.RawQuery = redactQuery(u.RawQuery)
	return u.String()
}

// redactQuery masks sensitive parameter values in a raw (still encoded) query
// string. It splits on both '&' and ';' so a secret is masked even in a
// segment that Go's query parser would reject.
func redactQuery(raw string) string {
	if raw == "" {
		return raw
	}
	var b strings.Builder
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] != '&' && raw[i] != ';' {
			continue
		}
		b.WriteString(redactQueryPair(raw[start:i]))
		if i < len(raw) {
			b.WriteByte(raw[i])
		}
		start = i + 1
	}
	return b.String()
}

func redactQueryPair(pair string) string {
	key, _, hasValue := strings.Cut(pair, "=")
	name, err := url.QueryUnescape(key)
	if err != nil {
		// The key cannot be decoded, so it cannot be ruled out as a secret.
		return redactedValue
	}
	if !hasValue || !sensitiveQueryParams[strings.ToLower(name)] {
		return pair
	}
	return key + "=" + redactedValue
}

// Summary returns loggable key/value pairs with secrets redacted.
func (c Config) Summary() map[string]string {
	db := c.RedactedDatabaseURL()
	if db == "" {
		db = "(not configured)"
	}
	return map[string]string{
		"log_level":         c.LogLevel,
		"log_format":        c.LogFormat,
		"http_addr":         c.HTTP.Addr,
		"database_url":      db,
		"shutdown_timeout":  c.ShutdownTimeout.String(),
		"readiness_timeout": c.ReadinessTimeout.String(),
	}
}
