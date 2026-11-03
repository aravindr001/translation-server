package logutil

import (
	"log/slog"
	"os"
)

// New returns a JSON logger writing to stdout. journald captures stdout, so no
// log files are opened by the gateway. The time key is named "timestamp" to
// match the operational log contract.
func New() *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				a.Key = "timestamp"
			}
			return a
		},
	})
	return slog.New(h)
}
