package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"hindimalayalamtranslator/internal/config"
)

// State is the worker lifecycle state exposed by /status.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateStopping State = "stopping"
)

var (
	// ErrWorkerUnavailable means a worker was expected to be ready but died or
	// left the ready state before the request could use it.
	ErrWorkerUnavailable = errors.New("worker unavailable")
	// ErrManagerClosed means the gateway is shutting down.
	ErrManagerClosed = errors.New("gateway shutting down")
)

// CooldownError is returned while a failed startup is cooling down.
type CooldownError struct {
	RetryAfter time.Duration
}

func (e *CooldownError) Error() string {
	return fmt.Sprintf("worker startup cooldown active for %s", e.RetryAfter)
}

// StartupError wraps a failed worker startup.
type StartupError struct {
	Err error
}

func (e *StartupError) Error() string { return "worker failed to start: " + e.Err.Error() }
func (e *StartupError) Unwrap() error { return e.Err }

// Status is the authenticated /status response.
type Status struct {
	State                State `json:"state"`
	SecondsUntilIdleStop int   `json:"seconds_until_idle_shutdown"`
	InFlight             int   `json:"-"`
}

type startAttempt struct {
	done   chan struct{}
	err    error
	closed bool
}

type process struct {
	cmd     *exec.Cmd
	pid     int
	done    chan struct{}
	waitErr error
	started time.Time
}

// Manager owns the single worker process. All state transitions are protected
// by mu; single-flight startup and stop finalization use attempt/stop channels.
type Manager struct {
	cfg    config.Config
	log    *slog.Logger
	client *Client
	now    func() time.Time

	mu            sync.Mutex
	cond          *sync.Cond
	state         State
	proc          *process
	attempt       *startAttempt
	stopDone      chan struct{}
	lastActivity  time.Time
	cooldownUntil time.Time
	inFlight      int
	closed        bool

	closeOnce sync.Once
	done      chan struct{}
}

// NewManager creates a manager, removes a stale socket, and starts the idle
// monitor. It never starts a worker.
func NewManager(cfg config.Config, log *slog.Logger) *Manager {
	m := &Manager{
		cfg:          cfg,
		log:          log,
		client:       NewClient(cfg.SocketPath),
		now:          time.Now,
		state:        StateStopped,
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}
	m.cond = sync.NewCond(&m.mu)

	// The systemd RuntimeDirectory is recreated on every boot. These operations
	// also make direct/manual starts robust, and deliberately ignore an existing
	// stale socket instead of failing.
	if dir := filepath.Dir(cfg.SocketPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Warn("could not create worker socket directory", "error", err.Error())
		}
	}
	if err := os.Remove(cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		log.Warn("could not remove stale worker socket", "error", err.Error())
	}

	go m.idleMonitor()
	return m
}

// State returns the current lifecycle state.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Status returns the current state and countdown.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{State: m.state, InFlight: m.inFlight}
	if m.state == StateReady {
		remaining := m.cfg.IdleTimeout - m.now().Sub(m.lastActivity)
		if remaining < 0 {
			remaining = 0
		}
		s.SecondsUntilIdleStop = int(math.Ceil(remaining.Seconds()))
	}
	return s
}

// CooldownRemaining returns the remaining startup cooldown.
func (m *Manager) CooldownRemaining() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cooldownLocked(m.now())
}

// Translate sends one request to the current worker. It is only valid after
// EnsureRunning returned nil, and the gateway serializes callers with a
// semaphore.
func (m *Manager) Translate(ctx context.Context, text string) (string, error) {
	return m.client.Translate(ctx, text)
}

// Begin marks a translate request as in flight. It resets the idle timer.
func (m *Manager) Begin() {
	m.mu.Lock()
	m.inFlight++
	m.lastActivity = m.now()
	m.mu.Unlock()
}

// End marks a translate request as complete. It resets the idle timer.
func (m *Manager) End() {
	m.mu.Lock()
	if m.inFlight > 0 {
		m.inFlight--
	}
	m.lastActivity = m.now()
	m.mu.Unlock()
}

// EnsureRunning waits for a ready worker, starting one if necessary. Concurrent
// callers share one startup attempt. A request that arrives during stopping
// waits for the stop to complete and then joins/creates a fresh startup.
func (m *Manager) EnsureRunning(ctx context.Context) error {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return ErrManagerClosed
		}
		switch m.state {
		case StateReady:
			m.lastActivity = m.now()
			m.mu.Unlock()
			return nil

		case StateStopping:
			ch := m.stopDone
			m.mu.Unlock()
			if err := waitChannel(ctx, ch); err != nil {
				return err
			}
			continue

		case StateStarting:
			a := m.attempt
			m.mu.Unlock()
			if a == nil {
				return ErrWorkerUnavailable
			}
			if err := waitChannel(ctx, a.done); err != nil {
				return err
			}
			m.mu.Lock()
			err := a.err
			state := m.state
			m.mu.Unlock()
			if err != nil {
				return err
			}
			if state == StateReady {
				return nil
			}
			return ErrWorkerUnavailable

		case StateStopped:
			if retry := m.cooldownLocked(m.now()); retry > 0 {
				m.mu.Unlock()
				return &CooldownError{RetryAfter: retry}
			}
			a := m.startLocked()
			m.mu.Unlock()
			if err := waitChannel(ctx, a.done); err != nil {
				return err
			}
			m.mu.Lock()
			err := a.err
			state := m.state
			m.mu.Unlock()
			if err != nil {
				return err
			}
			if state == StateReady {
				return nil
			}
			return ErrWorkerUnavailable

		default:
			m.mu.Unlock()
			return ErrWorkerUnavailable
		}
	}
}

// Warmup triggers a background start when stopped and resets the idle timer. It
// returns immediately with the state at that moment. If a stop is in progress,
// a start is scheduled for after the stop completes.
func (m *Manager) Warmup() State {
	m.mu.Lock()
	if m.closed {
		s := m.state
		m.mu.Unlock()
		return s
	}
	m.lastActivity = m.now()
	switch m.state {
	case StateStopped:
		if m.cooldownLocked(m.now()) == 0 {
			m.startLocked()
			m.mu.Unlock()
			return StateStarting
		}
		s := m.state
		m.mu.Unlock()
		return s
	case StateStopping:
		ch := m.stopDone
		m.mu.Unlock()
		go func() {
			<-ch
			ctx, cancel := context.WithTimeout(context.Background(), m.cfg.StartupTimeout+m.cfg.ColdStartWait)
			defer cancel()
			_ = m.EnsureRunning(ctx)
		}()
		return StateStopping
	default:
		s := m.state
		m.mu.Unlock()
		return s
	}
}

// ForceStop begins an asynchronous stop. It is used for a hung translation. It
// is a no-op unless the worker is ready.
func (m *Manager) ForceStop(reason string) {
	m.mu.Lock()
	if m.state != StateReady || m.proc == nil {
		m.mu.Unlock()
		return
	}
	p := m.proc
	m.state = StateStopping
	m.stopDone = make(chan struct{})
	// A worker that exceeded the translation deadline may be wedged. Cool down
	// after it is reaped so an immediate request cannot spin up another one in a
	// tight loop.
	m.cooldownUntil = m.now().Add(m.cfg.StartupCooldown)
	m.mu.Unlock()

	m.log.Info("worker state transition", "from", StateReady, "to", StateStopping, "reason", reason)
	go m.doStop(p, reason)
}

// Shutdown stops the worker and waits for it to be reaped. It is safe to call
// multiple times.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.closeOnce.Do(func() { close(m.done) })
	m.mu.Unlock()

	for {
		m.mu.Lock()
		switch m.state {
		case StateStopped:
			m.mu.Unlock()
			return nil
		case StateStopping:
			ch := m.stopDone
			m.mu.Unlock()
			if err := waitChannel(ctx, ch); err != nil {
				return err
			}
		case StateReady:
			p := m.proc
			m.state = StateStopping
			m.stopDone = make(chan struct{})
			ch := m.stopDone
			m.mu.Unlock()
			m.log.Info("worker state transition", "from", StateReady, "to", StateStopping, "reason", "gateway_shutdown")
			go m.doStop(p, "gateway_shutdown")
			if err := waitChannel(ctx, ch); err != nil {
				return err
			}
		case StateStarting:
			p := m.proc
			a := m.attempt
			m.mu.Unlock()
			if p != nil {
				_ = signalGroup(p, syscall.SIGKILL)
			}
			if a != nil {
				if err := waitChannel(ctx, a.done); err != nil {
					return err
				}
			} else {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(50 * time.Millisecond):
				}
			}
		default:
			m.mu.Unlock()
			return nil
		}
	}
}

func (m *Manager) startLocked() *startAttempt {
	a := &startAttempt{done: make(chan struct{})}
	m.state = StateStarting
	m.attempt = a
	m.cooldownUntil = time.Time{}
	m.lastActivity = m.now()
	go m.runStart(a)
	return a
}

func (m *Manager) runStart(a *startAttempt) {
	p, err := m.spawn()
	if err != nil {
		m.failStart(a, nil, fmt.Errorf("spawn worker: %w", err))
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// Start the reaper before failStart waits on p.done. The process may have
		// been spawned just as shutdown began.
		go m.waitProcess(p)
		m.failStart(a, p, ErrManagerClosed)
		return
	}
	m.proc = p
	m.mu.Unlock()

	m.log.Info("worker state transition", "from", StateStopped, "to", StateStarting, "pid", p.pid)
	go m.waitProcess(p)

	deadline := m.now().Add(m.cfg.StartupTimeout)
	ticker := time.NewTicker(ReadyPollInterval)
	defer ticker.Stop()

	for {
		if processDone(p) {
			m.failStart(a, p, errors.New("worker exited during startup"))
			return
		}
		if !m.now().Before(deadline) {
			m.failStart(a, p, errors.New("worker readiness timeout"))
			return
		}

		reqCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ready, readyErr := m.client.Ready(reqCtx)
		cancel()
		if ready && !processDone(p) {
			m.mu.Lock()
			if m.closed || m.state != StateStarting || m.proc != p || processDone(p) {
				m.mu.Unlock()
				m.failStart(a, p, ErrManagerClosed)
				return
			}
			m.state = StateReady
			m.lastActivity = m.now()
			a.err = nil
			a.closed = true
			close(a.done)
			m.cond.Broadcast()
			cold := m.now().Sub(p.started)
			m.mu.Unlock()

			m.log.Info("worker state transition", "from", StateStarting, "to", StateReady,
				"pid", p.pid, "cold_start_ms", cold.Milliseconds())
			return
		}
		if readyErr != nil && processDone(p) {
			m.failStart(a, p, errors.New("worker exited during startup"))
			return
		}

		select {
		case <-p.done:
			m.failStart(a, p, errors.New("worker exited during startup"))
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) failStart(a *startAttempt, p *process, cause error) {
	if p != nil {
		_ = signalGroup(p, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(m.cfg.ShutdownGrace):
			m.log.Error("worker did not exit after SIGKILL during startup cleanup", "pid", p.pid)
		}
	}

	m.mu.Lock()
	if m.proc == p {
		m.proc = nil
	}
	// Always complete the startup attempt, even if a concurrent unexpected exit
	// already moved the state to stopped. Otherwise callers waiting on a.done
	// would block until their own context deadline.
	if !a.closed {
		a.closed = true
		a.err = &StartupError{Err: cause}
		close(a.done)
		m.cond.Broadcast()
	}
	transitioned := m.state == StateStarting
	if transitioned {
		m.state = StateStopped
		m.cooldownUntil = m.now().Add(m.cfg.StartupCooldown)
	}
	m.mu.Unlock()
	if transitioned {
		m.log.Error("worker state transition", "from", StateStarting, "to", StateStopped,
			"reason", "startup_failed", "error", cause.Error(), "cooldown_s", int(m.cfg.StartupCooldown.Seconds()))
	}
}

func (m *Manager) spawn() (*process, error) {
	script := m.cfg.WorkerScript
	if !filepath.IsAbs(script) {
		abs, err := filepath.Abs(script)
		if err != nil {
			return nil, fmt.Errorf("resolve worker script: %w", err)
		}
		script = abs
	}
	dir := filepath.Dir(script)
	if err := os.MkdirAll(filepath.Dir(m.cfg.SocketPath), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if err := os.Remove(m.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	cmd := exec.Command(m.cfg.WorkerPython, script)
	cmd.Dir = dir
	cmd.Env = append(filteredEnv("TRANSLATOR_SOCKET"), "TRANSLATOR_SOCKET="+m.cfg.SocketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		done:    make(chan struct{}),
		started: started,
	}, nil
}

func (m *Manager) waitProcess(p *process) {
	p.waitErr = p.cmd.Wait()
	close(p.done)
	m.onProcessExit(p)
}

func (m *Manager) onProcessExit(p *process) {
	m.mu.Lock()
	if m.proc != p {
		m.mu.Unlock()
		return
	}
	switch m.state {
	case StateStarting:
		// runStart observes p.done and performs the failure transition.
		m.mu.Unlock()
		return
	case StateStopping:
		// doStop reaps and performs the final transition.
		m.mu.Unlock()
		return
	case StateReady:
		m.proc = nil
		m.state = StateStopped
		// An unexpected exit is also a failed start from the caller's point of
		// view. Cooling down prevents a crash loop from spawning without bound.
		m.cooldownUntil = m.now().Add(m.cfg.StartupCooldown)
		m.lastActivity = m.now()
		m.cond.Broadcast()
		err := p.waitErr
		cooldown := m.cfg.StartupCooldown
		m.mu.Unlock()
		m.log.Error("worker state transition", "from", StateReady, "to", StateStopped,
			"reason", "worker_exit", "pid", p.pid, "exit_error", errString(err),
			"cooldown_s", int(cooldown.Seconds()))
		return
	default:
		m.proc = nil
		m.mu.Unlock()
	}
}

func (m *Manager) doStop(p *process, reason string) {
	if p == nil {
		m.finishStop(nil)
		return
	}
	_ = signalGroup(p, syscall.SIGTERM)

	select {
	case <-p.done:
	case <-time.After(m.cfg.ShutdownGrace):
		m.log.Warn("worker did not exit after SIGTERM; sending SIGKILL", "pid", p.pid, "reason", reason)
		_ = signalGroup(p, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			m.log.Error("worker was not reaped after SIGKILL", "pid", p.pid, "reason", reason)
		}
	}

	m.finishStop(p)
}

func (m *Manager) finishStop(p *process) {
	m.mu.Lock()
	if p != nil && m.proc == p {
		m.proc = nil
	}
	if m.state == StateStopping {
		m.state = StateStopped
		if m.stopDone != nil {
			close(m.stopDone)
			m.stopDone = nil
		}
		m.lastActivity = m.now()
		m.cond.Broadcast()
	}
	m.mu.Unlock()
	if p != nil {
		m.log.Info("worker state transition", "from", StateStopping, "to", StateStopped,
			"reason", "stopped", "pid", p.pid)
	}
}

func (m *Manager) idleMonitor() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.maybeIdleStop()
		}
	}
}

func (m *Manager) maybeIdleStop() {
	m.mu.Lock()
	if m.closed || m.state != StateReady || m.inFlight != 0 {
		m.mu.Unlock()
		return
	}
	if m.now().Sub(m.lastActivity) < m.cfg.IdleTimeout {
		m.mu.Unlock()
		return
	}
	p := m.proc
	if p == nil {
		m.state = StateStopped
		m.mu.Unlock()
		return
	}
	m.state = StateStopping
	m.stopDone = make(chan struct{})
	m.mu.Unlock()

	m.log.Info("worker state transition", "from", StateReady, "to", StateStopping, "reason", "idle_timeout")
	go m.doStop(p, "idle_timeout")
}

func (m *Manager) cooldownLocked(now time.Time) time.Duration {
	if m.cooldownUntil.IsZero() {
		return 0
	}
	if !now.Before(m.cooldownUntil) {
		m.cooldownUntil = time.Time{}
		return 0
	}
	return ceilSecond(m.cooldownUntil.Sub(now))
}

func waitChannel(ctx context.Context, ch <-chan struct{}) error {
	if ch == nil {
		return ErrWorkerUnavailable
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func processDone(p *process) bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func signalGroup(p *process, sig syscall.Signal) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-p.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func filteredEnv(overridden string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+1)
	prefix := overridden + "="
	for _, e := range src {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		out = append(out, e)
	}
	return out
}

func ceilSecond(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	sec := int(math.Ceil(d.Seconds()))
	if sec < 1 {
		sec = 1
	}
	return time.Duration(sec) * time.Second
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
