package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"hindimalayalamtranslator/internal/auth"
	"hindimalayalamtranslator/internal/config"
	"hindimalayalamtranslator/internal/ratelimit"
	"hindimalayalamtranslator/internal/worker"
)

type contextKey struct{}

type requestInfo struct {
	keyName    string
	inputChars int
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// Server owns the public HTTP API, authentication, rate limiting, and the
// on-demand worker manager. It never loads a model itself.
type Server struct {
	cfg   config.Config
	log   *slog.Logger
	mgr   *worker.Manager
	auth  *auth.Store
	keys  *ratelimit.KeyLimiter
	fails *ratelimit.FailLimiter

	queue chan struct{}
	sem   chan struct{}

	headersOnce sync.Once
	handler     http.Handler
}

// New constructs the gateway. A missing or invalid key file is logged but does
// not prevent startup: the service then fails closed (all authenticated
// endpoints return 401) while /healthz remains available. SIGHUP can load a
// corrected file without a restart.
func New(cfg config.Config, log *slog.Logger) *Server {
	store, err := auth.NewStore(cfg.KeyFile)
	if err != nil {
		log.Error("key file not loaded; authenticated endpoints will fail closed",
			"path", cfg.KeyFile, "error", err.Error())
	}

	s := &Server{
		cfg:   cfg,
		log:   log,
		mgr:   worker.NewManager(cfg, log),
		auth:  store,
		keys:  ratelimit.NewKeyLimiter(cfg.RateLimitPerMinute, cfg.RateLimitBurst),
		fails: ratelimit.NewFailLimiter(cfg.AuthFailLimit, cfg.AuthFailWindow, cfg.AuthBlockDuration),
		queue: make(chan struct{}, cfg.QueueDepth+1),
		sem:   make(chan struct{}, 1),
	}
	s.keys.Retain(store.Names())

	mux := http.NewServeMux()
	mux.HandleFunc("/translate", s.handleTranslate)
	mux.HandleFunc("/transcribe", s.handleTranscribe)
	mux.HandleFunc("/warmup", s.handleWarmup)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/", s.handleNotFound)
	s.handler = s.instrument(mux)

	if store.Count() == 0 {
		log.Warn("no active API keys loaded", "path", cfg.KeyFile)
	} else {
		log.Info("API keys loaded", "count", store.Count())
	}
	log.Info("gateway configured",
		"port", cfg.Port,
		"socket", cfg.SocketPath,
		"idle_timeout", cfg.IdleTimeout.String(),
		"worker_startup_timeout", cfg.StartupTimeout.String(),
		"cold_start_wait", cfg.ColdStartWait.String(),
		"translate_timeout", cfg.TranslateTimeout.String(),
		"queue_depth", cfg.QueueDepth,
		"rate_limit_per_minute", cfg.RateLimitPerMinute,
	)
	return s
}

// Handler returns the wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Manager exposes the worker manager for tests and orderly shutdown.
func (s *Server) Manager() *worker.Manager { return s.mgr }

// ReloadKeys atomically reloads the key file. On failure the previous keys stay
// active.
func (s *Server) ReloadKeys() error {
	if err := s.auth.Reload(); err != nil {
		return err
	}
	s.keys.Retain(s.auth.Names())
	s.log.Info("API keys reloaded", "count", s.auth.Count())
	return nil
}

// Shutdown stops the worker and waits for reaping.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.mgr.Shutdown(ctx)
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &requestInfo{inputChars: -1}
		ctx := context.WithValue(r.Context(), contextKey{}, info)
		r = r.WithContext(ctx)

		if s.cfg.DebugHeaders {
			s.logHeadersOnce(r)
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic while handling request", "endpoint", r.URL.Path)
				if !sw.wrote {
					s.writeError(sw, http.StatusInternalServerError, "internal server error", 0)
				}
			}
			attrs := []any{
				"endpoint", r.URL.Path,
				"method", r.Method,
				"status", sw.status,
				"latency_ms", time.Since(start).Milliseconds(),
			}
			if info.keyName != "" {
				attrs = append(attrs, "key_name", info.keyName)
			}
			if info.inputChars >= 0 {
				attrs = append(attrs, "input_chars", info.inputChars)
			}
			s.log.Info("request", attrs...)
		}()

		next.ServeHTTP(sw, r)
	})
}

func (s *Server) logHeadersOnce(r *http.Request) {
	s.headersOnce.Do(func() {
		headers := make(map[string]string, len(r.Header))
		keys := make([]string, 0, len(r.Header))
		for k := range r.Header {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if sensitiveHeader(k) {
				headers[k] = "<redacted>"
				continue
			}
			headers[k] = strings.Join(r.Header.Values(k), ", ")
		}
		s.log.Info("debug request headers (first request)", "headers", headers, "remote_addr", r.RemoteAddr)
	})
}

func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key":
		return true
	default:
		return false
	}
}

func (s *Server) handleTranslate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, http.MethodPost)
		return
	}
	if r.ContentLength > s.cfg.MaxBodyBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large", 0)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)

	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large", 0)
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid request body", 0)
		return
	}
	if !utf8.Valid(body) {
		s.writeError(w, http.StatusBadRequest, "invalid utf-8", 0)
		return
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid json", 0)
		return
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		s.writeError(w, http.StatusBadRequest, "text is required", 0)
		return
	}
	chars := utf8.RuneCountInString(text)
	if chars > s.cfg.MaxTextRunes {
		s.writeError(w, http.StatusBadRequest, "text too long", 0)
		return
	}
	if info := requestInfoFrom(r); info != nil {
		info.inputChars = chars
	}

	// Past this point every request is authenticated, rate limited, and
	// validated. Queue admission happens before anything touches worker life,
	// so a rejected queue-full request cannot warm the model.
	select {
	case s.queue <- struct{}{}:
		defer func() { <-s.queue }()
	default:
		s.writeError(w, http.StatusTooManyRequests, "translation queue full", 2*time.Second)
		return
	}

	s.mgr.Begin()
	defer s.mgr.End()

	coldCtx, cancelCold := context.WithTimeout(context.Background(), s.cfg.ColdStartWait)
	err = s.mgr.EnsureRunning(coldCtx)
	cancelCold()
	if err != nil {
		s.writeWorkerUnavailable(w, err)
		return
	}

	queueCtx, cancelQueue := context.WithTimeout(context.Background(), s.cfg.TranslateTimeout)
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-queueCtx.Done():
		cancelQueue()
		s.writeError(w, http.StatusServiceUnavailable, "translation queue wait timed out", 5*time.Second)
		return
	}
	cancelQueue()

	tctx, cancelTranslate := context.WithTimeout(context.Background(), s.cfg.TranslateTimeout)
	translation, err := s.mgr.Translate(tctx, text)
	timedOut := errors.Is(tctx.Err(), context.DeadlineExceeded)
	cancelTranslate()
	if err != nil {
		if timedOut {
			s.mgr.ForceStop("translation_timeout")
			s.writeError(w, http.StatusServiceUnavailable, "translation timed out", 5*time.Second)
			return
		}
		s.writeError(w, http.StatusServiceUnavailable, "worker request failed", 5*time.Second)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"translation": translation})
}

func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, http.MethodPost)
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		return
	}

	if err := r.ParseMultipartForm(s.cfg.MaxAudioBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "audio too large", 0)
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid multipart form", 0)
		return
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "audio is required", 0)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid audio", 0)
		return
	}
	if len(data) == 0 {
		s.writeError(w, http.StatusBadRequest, "audio is required", 0)
		return
	}

	select {
	case s.queue <- struct{}{}:
		defer func() { <-s.queue }()
	default:
		s.writeError(w, http.StatusTooManyRequests, "translation queue full", 2*time.Second)
		return
	}

	s.mgr.Begin()
	defer s.mgr.End()

	coldCtx, cancelCold := context.WithTimeout(context.Background(), s.cfg.ColdStartWait)
	if err := s.mgr.EnsureRunning(coldCtx); err != nil {
		cancelCold()
		s.writeWorkerUnavailable(w, err)
		return
	}
	cancelCold()

	queueCtx, cancelQueue := context.WithTimeout(context.Background(), s.cfg.TranslateTimeout)
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-queueCtx.Done():
		cancelQueue()
		s.writeError(w, http.StatusServiceUnavailable, "translation queue wait timed out", 5*time.Second)
		return
	}
	cancelQueue()

	tctx, cancelTranslate := context.WithTimeout(context.Background(), s.cfg.TranslateTimeout)
	text, err := s.mgr.Transcribe(tctx, header.Filename, data)
	timedOut := errors.Is(tctx.Err(), context.DeadlineExceeded)
	cancelTranslate()
	if err != nil {
		if timedOut {
			s.mgr.ForceStop("translation_timeout")
			s.writeError(w, http.StatusServiceUnavailable, "translation timed out", 5*time.Second)
			return
		}
		s.writeError(w, http.StatusServiceUnavailable, "worker request failed", 5*time.Second)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (s *Server) handleWarmup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, http.MethodPost)
		return
	}
	if r.ContentLength > s.cfg.MaxBodyBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large", 0)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "request body too large", 0)
			return
		}
		s.writeError(w, http.StatusBadRequest, "invalid request body", 0)
		return
	}
	state := s.mgr.Warmup()
	s.writeJSON(w, http.StatusAccepted, map[string]string{"state": string(state)})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	st := s.mgr.Status()
	s.writeJSON(w, http.StatusOK, struct {
		State                string `json:"state"`
		SecondsUntilIdleStop int    `json:"seconds_until_idle_shutdown"`
	}{State: string(st.State), SecondsUntilIdleStop: st.SecondsUntilIdleStop})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusNotFound, "not found", 0)
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	s.writeError(w, http.StatusMethodNotAllowed, "method not allowed", 0)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	ip := s.clientIP(r)
	if blocked, retry := s.fails.Blocked(ip); blocked {
		s.writeError(w, http.StatusTooManyRequests, "too many failed authentication attempts", retry)
		return "", false
	}

	token, ok := parseBearer(r.Header.Get("Authorization"))
	if !ok {
		s.fails.Fail(ip)
		s.writeUnauthorized(w)
		return "", false
	}
	name, ok := s.auth.Authenticate(token)
	if !ok {
		s.fails.Fail(ip)
		s.writeUnauthorized(w)
		return "", false
	}

	allowed, retry := s.keys.Allow(name)
	if !allowed {
		s.writeError(w, http.StatusTooManyRequests, "rate limited", retry)
		return "", false
	}

	if info := requestInfoFrom(r); info != nil {
		info.keyName = name
	}
	return name, true
}

func parseBearer(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.ClientIPHeader != "" {
		if raw := r.Header.Get(s.cfg.ClientIPHeader); raw != "" {
			// Use the right-most hop. A trusted local proxy appends the address
			// it observed, so earlier values may be client-supplied.
			parts := strings.Split(raw, ",")
			last := strings.TrimSpace(parts[len(parts)-1])
			if ip := net.ParseIP(last); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}

func (s *Server) writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

func (s *Server) writeWorkerUnavailable(w http.ResponseWriter, err error) {
	retry := 5 * time.Second

	var cooldown *worker.CooldownError
	if errors.As(err, &cooldown) {
		retry = cooldown.RetryAfter
	}
	var startup *worker.StartupError
	if errors.As(err, &startup) {
		if remaining := s.mgr.CooldownRemaining(); remaining > 0 {
			retry = remaining
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		retry = 5 * time.Second
	}
	if errors.Is(err, worker.ErrManagerClosed) {
		retry = 5 * time.Second
	}
	s.writeError(w, http.StatusServiceUnavailable, "worker unavailable", retry)
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string, retryAfter time.Duration) {
	if retryAfter > 0 {
		sec := int(retryAfter / time.Second)
		if sec < 1 {
			sec = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(sec))
	}
	s.writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func requestInfoFrom(r *http.Request) *requestInfo {
	if v := r.Context().Value(contextKey{}); v != nil {
		if info, ok := v.(*requestInfo); ok {
			return info
		}
	}
	return nil
}
