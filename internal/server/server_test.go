package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hindimalayalamtranslator/internal/auth"
	"hindimalayalamtranslator/internal/config"
	"hindimalayalamtranslator/internal/server"
	"hindimalayalamtranslator/internal/worker"
)

const testText = "नमस्ते दुनिया।"

type testGateway struct {
	t        *testing.T
	srv      *server.Server
	http     *httptest.Server
	cfg      config.Config
	key      string
	dir      string
	spawnLog string
}

type result struct {
	status int
	header http.Header
	body   []byte
}

func (r result) errorMessage() string {
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(r.body, &payload)
	return payload.Error
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func pythonPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the fake worker")
	}
	return path
}

func writeKeyFile(t *testing.T, path string, keys ...auth.KeyEntry) {
	t.Helper()
	data, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestGateway(t *testing.T, mutate func(*config.Config), prepare func(dir string, cfg *config.Config)) *testGateway {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "worker.sock")
	keyFile := filepath.Join(dir, "keys.json")

	rawKey, entry, err := auth.GenerateKey("test")
	if err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, keyFile, entry)

	cfg := config.Default()
	cfg.Port = 0
	cfg.SocketPath = socket
	cfg.KeyFile = keyFile
	cfg.WorkerPython = pythonPath(t)
	cfg.WorkerScript = filepath.Join(repoRoot(t), "tests", "fake_worker.py")
	cfg.IdleTimeout = time.Minute
	cfg.StartupTimeout = 10 * time.Second
	cfg.ColdStartWait = 15 * time.Second
	cfg.TranslateTimeout = 10 * time.Second
	cfg.ShutdownGrace = 2 * time.Second
	cfg.StartupCooldown = 0
	cfg.MaxBodyBytes = 16 * 1024
	cfg.MaxTextRunes = 2000
	cfg.RateLimitPerMinute = 600
	cfg.RateLimitBurst = 600
	cfg.AuthFailLimit = 100
	cfg.AuthFailWindow = time.Minute
	cfg.AuthBlockDuration = time.Minute
	cfg.QueueDepth = 64

	spawnLog := filepath.Join(dir, "spawn.log")
	t.Setenv("FAKE_WORKER_SPAWN_LOG", spawnLog)
	t.Setenv("FAKE_READY_DELAY", "0")
	t.Setenv("FAKE_FAIL_START", "0")
	t.Setenv("FAKE_EXIT_AFTER_READY", "0")
	t.Setenv("FAKE_CRASH_AFTER_READY", "0")
	t.Setenv("FAKE_TRANSLATE_DELAY", "0")
	t.Setenv("FAKE_HANG_TRANSLATE", "0")
	t.Setenv("FAKE_CRASH_ON_TRANSLATE", "0")
	t.Setenv("FAKE_TERM_DELAY", "0")

	if mutate != nil {
		mutate(&cfg)
	}
	if prepare != nil {
		prepare(dir, &cfg)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := server.New(cfg, log)
	httpSrv := httptest.NewServer(srv.Handler())

	g := &testGateway{
		t:        t,
		srv:      srv,
		http:     httpSrv,
		cfg:      cfg,
		key:      rawKey,
		dir:      dir,
		spawnLog: spawnLog,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("gateway shutdown: %v", err)
		}
		httpSrv.Close()
	})
	return g
}

func (g *testGateway) do(method, path, key string, body []byte) (result, error) {
	if key == "" {
		return g.doAuth(method, path, "", body)
	}
	return g.doAuth(method, path, "Bearer "+key, body)
}

func (g *testGateway) doAuth(method, path, authorization string, body []byte) (result, error) {
	req, err := http.NewRequest(method, g.http.URL+path, bytes.NewReader(body))
	if err != nil {
		return result{}, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{}, err
	}
	return result{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// doChunked hides the concrete reader type so net/http sends the body with
// chunked transfer encoding instead of setting Content-Length.
func (g *testGateway) doChunked(path, key string, body []byte) (result, error) {
	reader := struct{ io.Reader }{Reader: bytes.NewReader(body)}
	req, err := http.NewRequest(http.MethodPost, g.http.URL+path, reader)
	if err != nil {
		return result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{}, err
	}
	return result{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func (g *testGateway) mustDo(method, path, key string, body []byte) result {
	g.t.Helper()
	res, err := g.do(method, path, key, body)
	if err != nil {
		g.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

func jsonBody(t *testing.T, text string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (g *testGateway) spawnPIDs() []int {
	g.t.Helper()
	data, err := os.ReadFile(g.spawnLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		g.t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	pids := make([]int, 0, len(lines))
	for _, line := range lines {
		pid, err := strconv.Atoi(line)
		if err != nil {
			g.t.Fatalf("bad spawn log line %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func (g *testGateway) requireSpawns(want int) {
	g.t.Helper()
	if got := len(g.spawnPIDs()); got != want {
		g.t.Fatalf("worker spawns = %d, want %d (pids %v)", got, want, g.spawnPIDs())
	}
}

func (g *testGateway) waitState(want worker.State, timeout time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if g.srv.Manager().State() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.t.Fatalf("worker state = %q, want %q after %s", g.srv.Manager().State(), want, timeout)
}

func TestUnauthenticatedAndInvalidRequestsNeverSpawn(t *testing.T) {
	g := newTestGateway(t, nil, nil)
	valid := jsonBody(t, testText)

	cases := []struct {
		name          string
		auth          string
		body          []byte
		wantStatus    int
		wantErrorText string
	}{
		{name: "missing key", body: valid, wantStatus: http.StatusUnauthorized},
		{name: "malformed auth", auth: "Basic abc", body: valid, wantStatus: http.StatusUnauthorized},
		{name: "bearer without token", auth: "Bearer", body: valid, wantStatus: http.StatusUnauthorized},
		{name: "wrong key", auth: "Bearer definitely-wrong", body: valid, wantStatus: http.StatusUnauthorized},
		{name: "oversized unauthenticated", body: bytes.Repeat([]byte("x"), 17*1024), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "invalid json", auth: "Bearer " + g.key, body: []byte("{not json"), wantStatus: http.StatusBadRequest},
		{name: "invalid utf8", auth: "Bearer " + g.key, body: []byte{'{', '"', 't', 'e', 'x', 't', '"', ':', '"', 0xff, '"', '}'}, wantStatus: http.StatusBadRequest},
		{name: "empty text", auth: "Bearer " + g.key, body: jsonBody(t, "   "), wantStatus: http.StatusBadRequest},
		{name: "over rune limit", auth: "Bearer " + g.key, body: jsonBody(t, strings.Repeat("अ", 2001)), wantStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := g.doAuth(http.MethodPost, "/translate", tc.auth, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", res.status, tc.wantStatus, res.body)
			}
		})
	}

	g.requireSpawns(0)
}

func TestOversizedBodyIsRejectedBeforeAuthentication(t *testing.T) {
	g := newTestGateway(t, nil, nil)
	res := g.mustDo(http.MethodPost, "/translate", "", bytes.Repeat([]byte("x"), 17*1024))
	if res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", res.status, res.body)
	}
	g.requireSpawns(0)
}

func TestChunkedOversizedBodyIsRejectedWithoutSpawning(t *testing.T) {
	g := newTestGateway(t, nil, nil)
	res, err := g.doChunked("/translate", g.key, bytes.Repeat([]byte("x"), 17*1024))
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked status = %d, want 413 (body %s)", res.status, res.body)
	}
	g.requireSpawns(0)
}

func TestIdenticalUnauthorizedResponses(t *testing.T) {
	g := newTestGateway(t, nil, nil)
	valid := jsonBody(t, testText)

	headers := []string{"", "Basic abc", "Bearer", "Bearer wrong-key"}
	for _, header := range headers {
		res, err := g.doAuth(http.MethodPost, "/translate", header, valid)
		if err != nil {
			t.Fatal(err)
		}
		if res.status != http.StatusUnauthorized {
			t.Fatalf("header %q: status = %d, want 401", header, res.status)
		}
		if string(res.body) != `{"error":"unauthorized"}` {
			t.Fatalf("header %q: body = %q, want identical unauthorized body", header, res.body)
		}
		if got := res.header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("header %q: Content-Type = %q", header, got)
		}
	}
	g.requireSpawns(0)
}

func TestRateLimitedRequestNeverSpawns(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.RateLimitPerMinute = 1
		cfg.RateLimitBurst = 1
	}, nil)

	// The first authenticated request consumes the only token but fails
	// validation, so it must not spawn a worker.
	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, ""))
	if res.status != http.StatusBadRequest {
		t.Fatalf("first status = %d, want 400 (body %s)", res.status, res.body)
	}

	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429 (body %s)", res.status, res.body)
	}
	if res.header.Get("Retry-After") == "" {
		t.Fatal("rate limited response is missing Retry-After")
	}
	g.requireSpawns(0)
}

func TestRateLimitAppliesToStatusIncludingAuthenticatedEndpoint(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.RateLimitPerMinute = 2
		cfg.RateLimitBurst = 2
	}, nil)

	for i := 0; i < 2; i++ {
		res := g.mustDo(http.MethodGet, "/status", g.key, nil)
		if res.status != http.StatusOK {
			t.Fatalf("status request %d = %d, want 200 (body %s)", i+1, res.status, res.body)
		}
	}
	res := g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("third status request = %d, want 429 (body %s)", res.status, res.body)
	}
	g.requireSpawns(0)
}

func TestFailedAuthLimiterBlocksAfterThreshold(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.AuthFailLimit = 3
		cfg.AuthFailWindow = 5 * time.Second
		cfg.AuthBlockDuration = 500 * time.Millisecond
	}, nil)

	for i := 0; i < 3; i++ {
		res := g.mustDo(http.MethodGet, "/status", "wrong-key", nil)
		if res.status != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401 (body %s)", i+1, res.status, res.body)
		}
	}
	res := g.mustDo(http.MethodGet, "/status", "wrong-key", nil)
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("request after threshold = %d, want 429 (body %s)", res.status, res.body)
	}
	if res.header.Get("Retry-After") == "" {
		t.Fatal("blocked response is missing Retry-After")
	}

	// A valid key from the same IP is also blocked while the cooldown is active.
	res = g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("valid key while blocked = %d, want 429", res.status)
	}

	time.Sleep(700 * time.Millisecond)
	res = g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusOK {
		t.Fatalf("valid key after cooldown = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(0)
}

func TestTwentyColdRequestsShareOneWorker(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.QueueDepth = 64
	}, nil)
	t.Setenv("FAKE_READY_DELAY", "0.7")

	const requests = 20
	body := jsonBody(t, testText)
	type response struct {
		res result
		err error
	}
	start := make(chan struct{})
	results := make(chan response, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := g.do(http.MethodPost, "/translate", g.key, body)
			results <- response{res: res, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	for r := range results {
		if r.err != nil {
			t.Fatalf("request error: %v", r.err)
		}
		if r.res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", r.res.status, r.res.body)
		}
		var payload struct {
			Translation string `json:"translation"`
		}
		if err := json.Unmarshal(r.res.body, &payload); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if payload.Translation != "ML:"+testText {
			t.Fatalf("translation = %q, want %q", payload.Translation, "ML:"+testText)
		}
	}
	g.requireSpawns(1)
	g.waitState(worker.StateReady, time.Second)
}

func TestIdleShutdownAndRestart(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.IdleTimeout = 250 * time.Millisecond
	}, nil)

	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("first translation = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(1)

	res = g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	var status struct {
		State                string `json:"state"`
		SecondsUntilIdleStop int    `json:"seconds_until_idle_shutdown"`
	}
	if err := json.Unmarshal(res.body, &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" {
		t.Fatalf("state = %q, want ready", status.State)
	}
	if status.SecondsUntilIdleStop < 0 || status.SecondsUntilIdleStop > 1 {
		t.Fatalf("seconds_until_idle_shutdown = %d, want 0 or 1", status.SecondsUntilIdleStop)
	}

	g.waitState(worker.StateStopped, 5*time.Second)

	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("restart translation = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(2)
}

func TestIdleMonitorDoesNotStopDuringTranslation(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.IdleTimeout = 150 * time.Millisecond
		cfg.TranslateTimeout = 5 * time.Second
	}, nil)
	t.Setenv("FAKE_TRANSLATE_DELAY", "0.8")

	done := make(chan result, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := g.do(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
		if err != nil {
			errs <- err
			return
		}
		done <- res
	}()

	time.Sleep(400 * time.Millisecond)
	if state := g.srv.Manager().State(); state != worker.StateReady {
		t.Fatalf("state during in-flight translation = %q, want ready", state)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case res := <-done:
		if res.status != http.StatusOK {
			t.Fatalf("slow translation = %d, want 200 (body %s)", res.status, res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow translation did not finish")
	}
	g.requireSpawns(1)

	// After the response, the idle timer is allowed to stop the worker.
	g.waitState(worker.StateStopped, 5*time.Second)
}

func TestRequestDuringStoppingWaitsThenStartsFreshWorker(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.IdleTimeout = 250 * time.Millisecond
		cfg.ShutdownGrace = 3 * time.Second
	}, nil)
	t.Setenv("FAKE_TERM_DELAY", "0.8")

	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("first translation = %d, want 200 (body %s)", res.status, res.body)
	}
	g.waitState(worker.StateStopping, 4*time.Second)

	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("translation during stopping = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(2)
	g.waitState(worker.StateReady, 2*time.Second)
}

func TestUnexpectedCrashAppliesCooldown(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.StartupCooldown = 700 * time.Millisecond
	}, nil)
	t.Setenv("FAKE_CRASH_ON_TRANSLATE", "1")

	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("crash translation = %d, want 503 (body %s)", res.status, res.body)
	}
	g.waitState(worker.StateStopped, 3*time.Second)
	g.requireSpawns(1)

	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("cooldown request = %d, want 503 (body %s)", res.status, res.body)
	}
	if res.header.Get("Retry-After") == "" {
		t.Fatal("cooldown response is missing Retry-After")
	}
	g.requireSpawns(1)

	t.Setenv("FAKE_CRASH_ON_TRANSLATE", "0")
	time.Sleep(800 * time.Millisecond)
	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("translation after cooldown = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(2)
}

func TestStartupFailureCooldownAndRecovery(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.StartupCooldown = 600 * time.Millisecond
		cfg.StartupTimeout = 5 * time.Second
	}, nil)
	t.Setenv("FAKE_FAIL_START", "1")

	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("failed startup = %d, want 503 (body %s)", res.status, res.body)
	}
	if res.header.Get("Retry-After") == "" {
		t.Fatal("startup failure is missing Retry-After")
	}
	g.waitState(worker.StateStopped, 2*time.Second)
	g.requireSpawns(1)

	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("request during startup cooldown = %d, want 503 (body %s)", res.status, res.body)
	}
	g.requireSpawns(1)

	t.Setenv("FAKE_FAIL_START", "0")
	time.Sleep(700 * time.Millisecond)
	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("recovery translation = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(2)
}

func TestTranslationTimeoutKillsWorker(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.TranslateTimeout = 400 * time.Millisecond
		cfg.ShutdownGrace = 500 * time.Millisecond
	}, nil)
	t.Setenv("FAKE_HANG_TRANSLATE", "1")

	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("timeout translation = %d, want 503 (body %s)", res.status, res.body)
	}
	if !strings.Contains(res.errorMessage(), "timed out") {
		t.Fatalf("error = %q, want translation timed out", res.errorMessage())
	}

	g.waitState(worker.StateStopped, 5*time.Second)
	pids := g.spawnPIDs()
	if len(pids) != 1 {
		t.Fatalf("spawns = %d, want 1", len(pids))
	}
	if err := syscall.Kill(pids[0], 0); err != syscall.ESRCH {
		t.Fatalf("worker pid %d still exists after timeout (kill error %v)", pids[0], err)
	}

	t.Setenv("FAKE_HANG_TRANSLATE", "0")
	res = g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusOK {
		t.Fatalf("translation after timeout restart = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(2)
}

func TestQueueFullReturns429WithoutSpawningExtraWorker(t *testing.T) {
	g := newTestGateway(t, func(cfg *config.Config) {
		cfg.QueueDepth = 0
		cfg.StartupTimeout = 5 * time.Second
	}, nil)
	t.Setenv("FAKE_READY_DELAY", "0.6")
	t.Setenv("FAKE_TRANSLATE_DELAY", "0.4")

	type response struct {
		res result
		err error
	}
	first := make(chan response, 1)
	go func() {
		res, err := g.do(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
		first <- response{res: res, err: err}
	}()
	time.Sleep(150 * time.Millisecond)

	res := g.mustDo(http.MethodPost, "/translate", g.key, jsonBody(t, testText))
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("queued request = %d, want 429 (body %s)", res.status, res.body)
	}
	if !strings.Contains(res.errorMessage(), "queue full") {
		t.Fatalf("error = %q, want queue full", res.errorMessage())
	}

	got := <-first
	if got.err != nil {
		t.Fatalf("first request: %v", got.err)
	}
	if got.res.status != http.StatusOK {
		t.Fatalf("first request = %d, want 200 (body %s)", got.res.status, got.res.body)
	}
	g.requireSpawns(1)
}

func TestWarmupStartsWorkerAndReturnsAccepted(t *testing.T) {
	g := newTestGateway(t, nil, nil)
	t.Setenv("FAKE_READY_DELAY", "0.5")

	res := g.mustDo(http.MethodPost, "/warmup", g.key, nil)
	if res.status != http.StatusAccepted {
		t.Fatalf("warmup = %d, want 202 (body %s)", res.status, res.body)
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(res.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "starting" {
		t.Fatalf("warmup state = %q, want starting", payload.State)
	}
	g.waitState(worker.StateReady, 3*time.Second)
	g.requireSpawns(1)

	res = g.mustDo(http.MethodGet, "/warmup", g.key, nil)
	if res.status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /warmup = %d, want 405", res.status)
	}
}

func TestBootWithStaleSocketAndNoWorker(t *testing.T) {
	var socketPath string
	g := newTestGateway(t, nil, func(dir string, cfg *config.Config) {
		socketPath = cfg.SocketPath
		if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("stale socket still exists: %v", err)
	}

	res := g.mustDo(http.MethodGet, "/healthz", "", nil)
	if res.status != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 (body %s)", res.status, res.body)
	}

	res = g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusOK {
		t.Fatalf("/status = %d, want 200 (body %s)", res.status, res.body)
	}
	var status struct {
		State                string `json:"state"`
		SecondsUntilIdleStop *int   `json:"seconds_until_idle_shutdown"`
	}
	if err := json.Unmarshal(res.body, &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "stopped" {
		t.Fatalf("state = %q, want stopped", status.State)
	}
	if status.SecondsUntilIdleStop == nil || *status.SecondsUntilIdleStop != 0 {
		t.Fatalf("seconds_until_idle_shutdown = %v, want 0", status.SecondsUntilIdleStop)
	}
	g.requireSpawns(0)
}

func TestMissingKeyFileFailsClosedAndCanBeReloaded(t *testing.T) {
	missing := ""
	g := newTestGateway(t, func(cfg *config.Config) {
		missing = filepath.Join(cfg.KeyFile+".missing", "keys.json")
		cfg.KeyFile = missing
	}, nil)

	res := g.mustDo(http.MethodGet, "/healthz", "", nil)
	if res.status != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", res.status)
	}
	res = g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusUnauthorized {
		t.Fatalf("/status with no keys loaded = %d, want 401", res.status)
	}

	if err := os.MkdirAll(filepath.Dir(missing), 0o700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(g.key))
	if err := os.WriteFile(missing, []byte(fmt.Sprintf(
		`{"keys":[{"name":"test","sha256_hash":"%s"}]}`+"\n",
		hex.EncodeToString(hash[:]),
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.srv.ReloadKeys(); err != nil {
		t.Fatalf("ReloadKeys: %v", err)
	}
	res = g.mustDo(http.MethodGet, "/status", g.key, nil)
	if res.status != http.StatusOK {
		t.Fatalf("/status after reload = %d, want 200 (body %s)", res.status, res.body)
	}
	g.requireSpawns(0)
}

func TestHealthzNeedsNoAuthentication(t *testing.T) {
	g := newTestGateway(t, nil, nil)
	res := g.mustDo(http.MethodGet, "/healthz", "", nil)
	if res.status != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 (body %s)", res.status, res.body)
	}
	if string(res.body) != `{"ok":true}` {
		t.Fatalf("/healthz body = %q", res.body)
	}
}
