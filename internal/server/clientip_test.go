package server

import (
	"net/http/httptest"
	"testing"

	"hindimalayalamtranslator/internal/config"
)

func TestClientIPUsesRightMostForwardedHop(t *testing.T) {
	s := &Server{cfg: config.Config{ClientIPHeader: "X-Forwarded-For"}}
	r := httptest.NewRequest("GET", "/status", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Fatalf("clientIP = %q, want the trusted last hop", got)
	}
}

func TestClientIPFallsBackToRemoteAddress(t *testing.T) {
	s := &Server{cfg: config.Config{ClientIPHeader: "X-Forwarded-For"}}
	r := httptest.NewRequest("GET", "/status", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := s.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("clientIP = %q, want remote fallback", got)
	}

	s.cfg.ClientIPHeader = ""
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("clientIP with disabled header = %q", got)
	}
}
