package logging

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// recentLines is how many log lines each binary keeps in memory for a
// failure's lock log excerpt (sair#97).
const recentLines = 500

// Recent holds the process's last log lines once KeepRecent has run.
var Recent = &Ring{max: recentLines}

// KeepRecent tees the default logger (which slog's default handler writes
// through) into Recent, so a failure can ship the lines that led up to it
// without shipping the whole log.
func KeepRecent() {
	log.SetOutput(io.MultiWriter(os.Stderr, Recent))
}

// Ring is a bounded buffer of log lines.
type Ring struct {
	mu    sync.Mutex
	max   int
	lines []string
	next  int // where the next line goes once lines is full
}

// Write stores p's lines; the log package hands over one entry per call.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if len(r.lines) < r.max {
			r.lines = append(r.lines, line)
			continue
		}
		r.lines[r.next] = line
		r.next = (r.next + 1) % r.max
	}
	return len(p), nil
}

// Lines returns, oldest first, the stored lines that contain any of match
// (all of them when match is empty), redacted. Filtering keeps another
// tenant's lock out of a guest's excerpt, and keeps the excerpt about the
// phone that failed.
func (r *Ring) Lines(match ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for i := range r.lines {
		line := r.lines[(r.next+i)%len(r.lines)]
		if len(match) == 0 || containsAny(line, match) {
			out = append(out, RedactSecrets(line))
		}
	}
	return out
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
