// Package sdnotify implements the subset of sd_notify(3) needed by the
// gateway: READY=1, STOPPING=1, and periodic WATCHDOG=1 datagrams. It uses only
// the standard library.
package sdnotify

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notify sends a state string to NOTIFY_SOCKET. It is a no-op when systemd did
// not set the variable.
func Notify(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	// Abstract namespace sockets are represented as @name by systemd.
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	}
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return fmt.Errorf("dial NOTIFY_SOCKET: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("write NOTIFY_SOCKET: %w", err)
	}
	return nil
}

// Ready sends READY=1.
func Ready() error { return Notify("READY=1") }

// Stopping sends STOPPING=1.
func Stopping() error { return Notify("STOPPING=1") }

// Watchdog sends WATCHDOG=1 every interval until ctx is cancelled. If
// WATCHDOG_USEC is unset, it computes interval/3 from fallback. A non-positive
// interval disables the watchdog.
func Watchdog(ctx context.Context, fallback time.Duration) {
	interval := fallback / 3
	if v := os.Getenv("WATCHDOG_USEC"); v != "" {
		if usec, err := strconv.ParseInt(v, 10, 64); err == nil && usec > 0 {
			interval = time.Duration(usec) * time.Microsecond / 3
		}
	}
	if interval <= 0 {
		return
	}
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A failure to notify is not fatal. systemd will restart the unit if
			// it is truly unable to receive watchdog pings.
			_ = Notify("WATCHDOG=1")
		}
	}
}
