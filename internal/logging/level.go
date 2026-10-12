// Package logging sets the binaries' log level from the environment.
package logging

import (
	"fmt"
	"log/slog"
	"os"
)

// SetLevelFromEnv applies SAIR_LOG_LEVEL (debug, info, warn or error; default
// info) to the default logger, so debug detail such as the device source's
// per-chunk forward byte counts can be switched on without a rebuild (sair#79).
func SetLevelFromEnv() {
	level, err := parseLevel(os.Getenv("SAIR_LOG_LEVEL"))
	if err != nil {
		slog.Warn("ignoring SAIR_LOG_LEVEL", "error", err)
		return
	}
	slog.SetLogLoggerLevel(level)
}

func parseLevel(v string) (slog.Level, error) {
	if v == "" {
		return slog.LevelInfo, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		return slog.LevelInfo, fmt.Errorf("%q is not debug, info, warn or error", v)
	}
	return level, nil
}
