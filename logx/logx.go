// Package logx is the router's small logging facade.
//
// It wraps log/slog so call sites stay short. Logger embeds the slog logger,
// so Debug/Info/Warn/Error are promoted and callable directly. The package
// lives on its own so config, proxy and router can log the same way without
// importing each other.
package logx

import (
	"log/slog"
	"os"
)

// Logger is a thin wrapper so call sites stay short.
type Logger struct{ *slog.Logger }

// New builds a text logger on stderr, at debug level when verbose.
func New(verbose bool) *Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return &Logger{slog.New(handler)}
}

// Std is the logger used by helpers that do not carry one around.
var Std = New(false)

// Info and Warn log through Std for those helpers.
func Info(msg string, args ...any) { Std.Info(msg, args...) }
func Warn(msg string, args ...any) { Std.Warn(msg, args...) }
