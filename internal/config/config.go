package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains every runtime tunable used by the gateway and the worker
// environment it spawns.
type Config struct {
	Port               int
	SocketPath         string
	KeyFile            string
	ClientIPHeader     string
	DebugHeaders       bool
	IdleTimeout        time.Duration
	StartupTimeout     time.Duration
	ColdStartWait      time.Duration
	TranslateTimeout   time.Duration
	ShutdownGrace      time.Duration
	StartupCooldown    time.Duration
	MaxBodyBytes       int64
	MaxAudioBytes      int64
	MaxTextRunes       int
	WhisperModel       string
	RateLimitPerMinute int
	RateLimitBurst     int
	AuthFailLimit      int
	AuthFailWindow     time.Duration
	AuthBlockDuration  time.Duration
	QueueDepth         int
	WorkerPython       string
	WorkerScript       string
}

// Default returns the configuration used when no environment variables are set.
func Default() Config {
	return Config{
		Port:               18080,
		SocketPath:         "/run/translator/worker.sock",
		KeyFile:            "/etc/translator/keys.json",
		ClientIPHeader:     "",
		DebugHeaders:       false,
		IdleTimeout:        10 * time.Minute,
		StartupTimeout:     120 * time.Second,
		ColdStartWait:      60 * time.Second,
		TranslateTimeout:   60 * time.Second,
		ShutdownGrace:      10 * time.Second,
		StartupCooldown:    30 * time.Second,
		MaxBodyBytes:       16 * 1024,
		MaxAudioBytes:      25 * 1024 * 1024,
		MaxTextRunes:       2000,
		WhisperModel:       "openai/whisper-small",
		RateLimitPerMinute: 30,
		RateLimitBurst:     30,
		AuthFailLimit:      10,
		AuthFailWindow:     time.Minute,
		AuthBlockDuration:  time.Minute,
		QueueDepth:         8,
		WorkerPython:       "/opt/translator/venv/bin/python",
		WorkerScript:       "/opt/translator/worker/worker.py",
	}
}

// Load reads configuration from TRANSLATOR_* environment variables over the
// defaults. Invalid values are reported rather than silently replaced.
func Load() (Config, error) {
	c := Default()
	var err error

	if c.Port, err = envInt("TRANSLATOR_PORT", c.Port); err != nil {
		return c, err
	}
	c.SocketPath = envString("TRANSLATOR_SOCKET_PATH", c.SocketPath)
	c.KeyFile = envString("TRANSLATOR_KEY_FILE", c.KeyFile)
	c.ClientIPHeader = strings.TrimSpace(envString("TRANSLATOR_CLIENT_IP_HEADER", c.ClientIPHeader))
	if c.DebugHeaders, err = envBool("TRANSLATOR_DEBUG_HEADERS", c.DebugHeaders); err != nil {
		return c, err
	}
	if c.IdleTimeout, err = envDuration("TRANSLATOR_IDLE_TIMEOUT", c.IdleTimeout); err != nil {
		return c, err
	}
	if c.StartupTimeout, err = envDuration("TRANSLATOR_STARTUP_TIMEOUT", c.StartupTimeout); err != nil {
		return c, err
	}
	if c.ColdStartWait, err = envDuration("TRANSLATOR_COLD_START_WAIT", c.ColdStartWait); err != nil {
		return c, err
	}
	if c.TranslateTimeout, err = envDuration("TRANSLATOR_TRANSLATE_TIMEOUT", c.TranslateTimeout); err != nil {
		return c, err
	}
	if c.ShutdownGrace, err = envDuration("TRANSLATOR_SHUTDOWN_GRACE", c.ShutdownGrace); err != nil {
		return c, err
	}
	if c.StartupCooldown, err = envDuration("TRANSLATOR_STARTUP_COOLDOWN", c.StartupCooldown); err != nil {
		return c, err
	}
	if c.MaxBodyBytes, err = envInt64("TRANSLATOR_MAX_BODY_BYTES", c.MaxBodyBytes); err != nil {
		return c, err
	}
	if c.MaxAudioBytes, err = envInt64("TRANSLATOR_MAX_AUDIO_BYTES", c.MaxAudioBytes); err != nil {
		return c, err
	}
	if c.MaxTextRunes, err = envInt("TRANSLATOR_MAX_TEXT_RUNES", c.MaxTextRunes); err != nil {
		return c, err
	}
	c.WhisperModel = strings.TrimSpace(envString("TRANSLATOR_WHISPER_MODEL", c.WhisperModel))
	if c.RateLimitPerMinute, err = envInt("TRANSLATOR_RATE_LIMIT_PER_MINUTE", c.RateLimitPerMinute); err != nil {
		return c, err
	}
	if c.RateLimitBurst, err = envInt("TRANSLATOR_RATE_LIMIT_BURST", c.RateLimitBurst); err != nil {
		return c, err
	}
	if c.AuthFailLimit, err = envInt("TRANSLATOR_AUTH_FAIL_LIMIT", c.AuthFailLimit); err != nil {
		return c, err
	}
	if c.AuthFailWindow, err = envDuration("TRANSLATOR_AUTH_FAIL_WINDOW", c.AuthFailWindow); err != nil {
		return c, err
	}
	if c.AuthBlockDuration, err = envDuration("TRANSLATOR_AUTH_BLOCK_DURATION", c.AuthBlockDuration); err != nil {
		return c, err
	}
	if c.QueueDepth, err = envInt("TRANSLATOR_QUEUE_DEPTH", c.QueueDepth); err != nil {
		return c, err
	}
	c.WorkerPython = envString("TRANSLATOR_WORKER_PYTHON", c.WorkerPython)
	c.WorkerScript = envString("TRANSLATOR_WORKER_SCRIPT", c.WorkerScript)

	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// Validate rejects configurations that cannot safely operate.
func (c Config) Validate() error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("TRANSLATOR_PORT must be between 0 and 65535")
	}
	if c.SocketPath == "" {
		return fmt.Errorf("TRANSLATOR_SOCKET_PATH must not be empty")
	}
	if c.KeyFile == "" {
		return fmt.Errorf("TRANSLATOR_KEY_FILE must not be empty")
	}
	if c.WorkerPython == "" || c.WorkerScript == "" {
		return fmt.Errorf("TRANSLATOR_WORKER_PYTHON and TRANSLATOR_WORKER_SCRIPT must not be empty")
	}
	if c.IdleTimeout <= 0 || c.StartupTimeout <= 0 || c.ColdStartWait <= 0 ||
		c.TranslateTimeout <= 0 || c.ShutdownGrace <= 0 || c.StartupCooldown < 0 {
		return fmt.Errorf("durations must be positive (startup cooldown may be zero)")
	}
	if c.AuthFailWindow <= 0 || c.AuthBlockDuration <= 0 {
		return fmt.Errorf("auth failure window and block duration must be positive")
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("TRANSLATOR_MAX_BODY_BYTES must be positive")
	}
	if c.MaxAudioBytes <= 0 {
		return fmt.Errorf("TRANSLATOR_MAX_AUDIO_BYTES must be positive")
	}
	if c.WhisperModel == "" {
		return fmt.Errorf("TRANSLATOR_WHISPER_MODEL must not be empty")
	}
	if c.MaxTextRunes <= 0 {
		return fmt.Errorf("TRANSLATOR_MAX_TEXT_RUNES must be positive")
	}
	if c.RateLimitPerMinute <= 0 || c.RateLimitBurst <= 0 {
		return fmt.Errorf("rate limit values must be positive")
	}
	if c.AuthFailLimit <= 0 {
		return fmt.Errorf("TRANSLATOR_AUTH_FAIL_LIMIT must be positive")
	}
	if c.QueueDepth < 0 {
		return fmt.Errorf("TRANSLATOR_QUEUE_DEPTH must be zero or positive")
	}
	return nil
}

func envString(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return n, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return fallback, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return n, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback, fmt.Errorf("%s must be a Go duration such as 30s or 10m: %w", name, err)
	}
	return d, nil
}

func envBool(name string, fallback bool) (bool, error) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return fallback, fmt.Errorf("%s must be true or false", name)
	}
}
