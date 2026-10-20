package logx

// logx is the logging facade the other three packages share, so what matters
// here is the one behaviour it actually decides: the level. Everything else is
// slog's, and testing slog would only test slog.

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// capture swaps os.Stderr for a pipe and returns a function that gives back
// everything written to it. New() binds the handler to os.Stderr at
// construction time, so the swap has to happen before the logger is built; the
// returned function closes the pipe and waits for the drain, which is what
// makes the read deterministic instead of a race with the test goroutine.
func capture(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = buf.ReadFrom(r)
	}()

	var finalized bool
	return func() string {
		if finalized {
			return buf.String()
		}
		finalized = true
		os.Stderr = orig
		_ = w.Close() // the logger still holds this file; closing it ends the drain
		<-done
		_ = r.Close()
		return buf.String()
	}
}

// The whole point of the verbose flag: -v has to open the Debug level, and
// leave it shut without the flag. A router with -v should show what it is
// doing per request; without it, that noise stays out of the way.
func TestNewRespectsVerbose(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verbose  bool
		wantLogs bool
	}{
		{"verbose enables debug", true, true},
		{"quiet drops debug", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finish := capture(t)
			New(tc.verbose).Debug("a debug line")
			got := finish()
			if (got != "") != tc.wantLogs {
				t.Errorf("Debug with verbose=%v wrote %q, want output: %v", tc.verbose, got, tc.wantLogs)
			}
		})
	}
}

// Info and above are never filtered, at either level: a warning that only
// shows up with -v is a warning most operators never see.
func TestInfoAndAboveAlwaysLog(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		finish := capture(t)
		log := New(verbose)
		log.Info("an info line")
		log.Warn("a warn line")
		log.Error("an error line")
		out := finish()
		for _, want := range []string{"an info line", "a warn line", "an error line"} {
			if !strings.Contains(out, want) {
				t.Errorf("verbose=%v: output %q is missing %q", verbose, out, want)
			}
		}
	}
}

// The level has to be readable, not just observable: -v is the only thing that
// separates the two, and a caller may want to check it without emitting a line.
func TestLevelIsSlogLevel(t *testing.T) {
	if got := New(true).Enabled(nil, slog.LevelDebug); !got {
		t.Error("New(true) should enable slog.LevelDebug")
	}
	if got := New(false).Enabled(nil, slog.LevelDebug); got {
		t.Error("New(false) should not enable slog.LevelDebug")
	}
	if got := New(false).Enabled(nil, slog.LevelWarn); !got {
		t.Error("New(false) should still enable slog.LevelWarn")
	}
}

// The package-level helpers exist so config, proxy and router can log without
// carrying a logger around. They go through Std, so they must respect whatever
// Std was replaced with.
func TestStdHelpersUseStd(t *testing.T) {
	orig := Std
	t.Cleanup(func() { Std = orig })

	finish := capture(t)
	Std = New(true)
	Info("through Std", "n", 1)
	Warn("also through Std")

	out := finish()
	for _, want := range []string{"through Std", "n=1", "also through Std"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
}

// Std is the default: helpers run before or without a logger, and they must
// not be debug-quiet by accident just because nobody asked for verbosity.
func TestStdDefaultsToInfoLevel(t *testing.T) {
	if got := Std.Enabled(nil, slog.LevelDebug); got {
		t.Error("the default Std should not emit debug lines")
	}
	if got := Std.Enabled(nil, slog.LevelInfo); !got {
		t.Error("the default Std should emit info lines")
	}
}

// Structured arguments survive the wrapper: the whole point of slog over fmt
// is that the router's counters stay machine-readable in the log.
func TestStructuredArgsArePreserved(t *testing.T) {
	finish := capture(t)
	New(true).Info("proxy list", "count", 200, "file", "/tmp/ports.txt")

	out := finish()
	for _, want := range []string{`count=200`, `file=/tmp/ports.txt`, "level=INFO"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
}
