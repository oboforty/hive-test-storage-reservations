// Package logging builds an slog.Logger that fans out to a log file and to
// stdout/stderr, with per-sink minimum levels depending on ENV.
//
//	ENV=DEBUG    file: debug+   std: debug+
//	ENV=TESTING  file: debug+   std: info+
//	ENV=PROD     file: info+    std: errors only
//
// On the std side, records below WARN go to stdout, WARN and above to stderr.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/oboforty/hiveproj/internal/config"
)

// New creates the application logger. The returned closer closes the log
// file and must be called on shutdown.
func New(cfg config.Config) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}

	var fileLevel, stdLevel slog.Level
	switch cfg.Env {
	case config.EnvDebug:
		fileLevel, stdLevel = slog.LevelDebug, slog.LevelDebug
	case config.EnvTesting:
		fileLevel, stdLevel = slog.LevelDebug, slog.LevelInfo
	default: // PROD
		fileLevel, stdLevel = slog.LevelInfo, slog.LevelError
	}

	fileHandler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: fileLevel, AddSource: !cfg.IsProd()})
	stdOpts := &slog.HandlerOptions{Level: stdLevel}
	std := &splitHandler{
		low:  slog.NewTextHandler(os.Stdout, stdOpts),
		high: slog.NewTextHandler(os.Stderr, stdOpts),
	}

	logger := slog.New(&fanoutHandler{handlers: []slog.Handler{fileHandler, std}})
	return logger, f, nil
}

// Discard returns a logger that drops everything (useful in tests).
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fanoutHandler dispatches each record to every handler that accepts its level.
type fanoutHandler struct{ handlers []slog.Handler }

func (h *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, x := range h.handlers {
		if x.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, x := range h.handlers {
		if x.Enabled(ctx, r.Level) {
			if err := x.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(h.handlers))
	for i, x := range h.handlers {
		out[i] = x.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: out}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(h.handlers))
	for i, x := range h.handlers {
		out[i] = x.WithGroup(name)
	}
	return &fanoutHandler{handlers: out}
}

// splitHandler routes records below WARN to low (stdout) and the rest to high (stderr).
type splitHandler struct{ low, high slog.Handler }

func (h *splitHandler) pick(l slog.Level) slog.Handler {
	if l >= slog.LevelWarn {
		return h.high
	}
	return h.low
}

func (h *splitHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.pick(l).Enabled(ctx, l)
}

func (h *splitHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.pick(r.Level).Handle(ctx, r)
}

func (h *splitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &splitHandler{low: h.low.WithAttrs(attrs), high: h.high.WithAttrs(attrs)}
}

func (h *splitHandler) WithGroup(name string) slog.Handler {
	return &splitHandler{low: h.low.WithGroup(name), high: h.high.WithGroup(name)}
}
