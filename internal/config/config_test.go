package config

import (
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config is invalid: %v", err)
	}
	if cfg.MaxBodyBytes != 16*1024 {
		t.Fatalf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
	if cfg.Port != 18080 {
		t.Fatalf("Port = %d, want 18080 (8080 is used on the target host)", cfg.Port)
	}
	if cfg.MaxTextRunes != 2000 {
		t.Fatalf("MaxTextRunes = %d", cfg.MaxTextRunes)
	}
	if cfg.IdleTimeout != 10*time.Minute {
		t.Fatalf("IdleTimeout = %s", cfg.IdleTimeout)
	}
}

func TestLoadOverridesFromEnvironment(t *testing.T) {
	t.Setenv("TRANSLATOR_PORT", "9090")
	t.Setenv("TRANSLATOR_IDLE_TIMEOUT", "90s")
	t.Setenv("TRANSLATOR_QUEUE_DEPTH", "3")
	t.Setenv("TRANSLATOR_DEBUG_HEADERS", "true")
	t.Setenv("TRANSLATOR_CLIENT_IP_HEADER", " X-Forwarded-For ")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 || cfg.IdleTimeout != 90*time.Second || cfg.QueueDepth != 3 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if !cfg.DebugHeaders {
		t.Fatal("DebugHeaders was not enabled")
	}
	if cfg.ClientIPHeader != "X-Forwarded-For" {
		t.Fatalf("ClientIPHeader = %q", cfg.ClientIPHeader)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Setenv("TRANSLATOR_IDLE_TIMEOUT", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("invalid duration was accepted")
	}

	t.Setenv("TRANSLATOR_IDLE_TIMEOUT", "")
	t.Setenv("TRANSLATOR_RATE_LIMIT_PER_MINUTE", "0")
	if _, err := Load(); err == nil {
		t.Fatal("zero rate limit was accepted")
	}
}

func TestValidationBounds(t *testing.T) {
	cfg := Default()
	cfg.Port = 70000
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid port was accepted")
	}
	cfg = Default()
	cfg.QueueDepth = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative queue depth was accepted")
	}
	cfg = Default()
	cfg.StartupCooldown = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative startup cooldown was accepted")
	}
	cfg = Default()
	cfg.StartupCooldown = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero startup cooldown should be allowed: %v", err)
	}
}
