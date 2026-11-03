package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hindimalayalamtranslator/internal/auth"
	"hindimalayalamtranslator/internal/config"
	"hindimalayalamtranslator/internal/logutil"
	"hindimalayalamtranslator/internal/sdnotify"
	"hindimalayalamtranslator/internal/server"
)

func main() {
	logger := logutil.New()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "genkey":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "usage: gateway genkey <name>")
				os.Exit(2)
			}
			if err := genkey(os.Args[2]); err != nil {
				fmt.Fprintln(os.Stderr, "genkey failed:", err)
				os.Exit(1)
			}
			return
		case "serve":
			// default mode; accepted explicitly for systemd clarity
		default:
			fmt.Fprintln(os.Stderr, "usage: gateway [serve] | gateway genkey <name>")
			os.Exit(2)
		}
	}

	if err := run(logger); err != nil {
		logger.Error("gateway exited with error", "error", err.Error())
		os.Exit(1)
	}
}

func genkey(name string) error {
	raw, entry, err := auth.GenerateKey(name)
	if err != nil {
		return err
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	// This is the only place the raw key is ever emitted. It is not logged.
	fmt.Printf("raw_key=%s\n", raw)
	fmt.Printf("%s\n", entryJSON)
	return nil
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	srv := server.New(cfg, logger)

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 * 1024,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	logger.Info("gateway listening", "address", ln.Addr().String(), "worker_state", "stopped")

	if err := sdnotify.Ready(); err != nil {
		logger.Warn("sd_notify READY failed", "error", err.Error())
	}
	watchdogCtx, cancelWatchdog := context.WithCancel(context.Background())
	defer cancelWatchdog()
	go sdnotify.Watchdog(watchdogCtx, 30*time.Second)

	serveErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case err := <-serveErr:
			if err != nil {
				shutdown(srv, httpSrv, logger)
				return fmt.Errorf("http server: %w", err)
			}
			shutdown(srv, httpSrv, logger)
			return nil
		case sig := <-sigs:
			switch sig {
			case syscall.SIGHUP:
				if err := srv.ReloadKeys(); err != nil {
					logger.Error("API key reload failed; keeping previous keys", "error", err.Error())
				}
			default:
				logger.Info("gateway shutting down", "signal", sig.String())
				_ = sdnotify.Stopping()
				cancelWatchdog()
				shutdown(srv, httpSrv, logger)
				return nil
			}
		}
	}
}

// shutdown gives active HTTP requests a short drain window, then stops the
// worker with a separate budget. systemd's TimeoutStopSec comfortably covers
// the combined 5s + 12s.
func shutdown(srv *server.Server, httpSrv *http.Server, logger *slog.Logger) {
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	_ = httpSrv.Shutdown(httpCtx)
	cancelHTTP()

	workerCtx, cancelWorker := context.WithTimeout(context.Background(), 12*time.Second)
	if err := srv.Shutdown(workerCtx); err != nil {
		logger.Warn("worker shutdown incomplete", "error", err.Error())
	}
	cancelWorker()
	_ = httpSrv.Close()
}
