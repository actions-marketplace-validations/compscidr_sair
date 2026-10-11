package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for v, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := parseLevel(v); err != nil || got != want {
			t.Errorf("parseLevel(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	if _, err := parseLevel("verbose"); err == nil {
		t.Error("parseLevel(verbose) should fail")
	}
}

func TestSetLevelFromEnvEnablesDebug(t *testing.T) {
	defer slog.SetLogLoggerLevel(slog.LevelInfo)
	t.Setenv("SAIR_LOG_LEVEL", "debug")
	SetLevelFromEnv()
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug is not enabled on the default logger")
	}
}
