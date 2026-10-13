package devicesource

import (
	"fmt"
	"github.com/compscidr/sair/internal/logging"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// defaultStallTimeout is how long a forward may go without progress before
	// it is torn down: a write to the adb server blocked that long, or an
	// install-write that moved no bytes either way (sair#79).
	defaultStallTimeout = 2 * time.Minute
	// defaultIdleWarnInterval is how often a forward with no traffic at all is
	// logged. Only logged: a long, silent test is legitimate.
	defaultIdleWarnInterval = 5 * time.Minute
)

// forwardWatch tracks one forward's progress so a stalled one fails fast
// instead of hanging until an outer CI timeout.
type forwardWatch struct {
	serial, service string
	lastIO          atomic.Int64 // unix nanos of the last byte either way
	writingSince    atomic.Int64 // unix nanos a write to the adb server began; 0 when none is pending
	reason          atomic.Pointer[string]
}

// newForwardWatch keeps the service redacted: it is only ever used in log
// lines and stall reasons, and a forwarded add-suggestion carries the
// passphrase (sair#100). isInstallWrite looks at the prefix, which stays.
func newForwardWatch(serial, service string) *forwardWatch {
	w := &forwardWatch{serial: serial, service: logging.RedactSecrets(service)}
	w.lastIO.Store(time.Now().UnixNano())
	return w
}

func (w *forwardWatch) writing() { w.writingSince.Store(time.Now().UnixNano()) }

func (w *forwardWatch) touch() {
	w.writingSince.Store(0)
	w.lastIO.Store(time.Now().UnixNano())
}

// stalled is why the watch tore the forward down, or "".
func (w *forwardWatch) stalled() string {
	if r := w.reason.Load(); r != nil {
		return *r
	}
	return ""
}

// run checks the forward until stop closes, closing conn when it stalls.
func (w *forwardWatch) run(conn io.Closer, stallTimeout, warnInterval time.Duration, stop <-chan struct{}) {
	if stallTimeout <= 0 {
		stallTimeout = defaultStallTimeout
	}
	if warnInterval <= 0 {
		warnInterval = defaultIdleWarnInterval
	}
	tick := time.NewTicker(min(stallTimeout, warnInterval) / 4)
	defer tick.Stop()
	warned := time.Now()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			if r := stallReason(w.service, now, w.lastIO.Load(), w.writingSince.Load(), stallTimeout); r != "" {
				w.reason.Store(&r)
				conn.Close()
				return
			}
			if idle := now.Sub(time.Unix(0, w.lastIO.Load())); idle >= warnInterval && now.Sub(warned) >= warnInterval {
				slog.Warn("ForwardToDevice: no traffic", "serial", w.serial, "command", w.service, "idle", idle.Round(time.Second))
				warned = now
			}
		}
	}
}

// stallReason says why a forward counts as stalled at now, or "" if it does
// not. A write the adb server has not accepted for timeout is a stall for any
// service. An install-write idle for timeout is one too: it only copies bytes,
// unlike a long-running test, which may legitimately say nothing for minutes.
func stallReason(service string, now time.Time, lastIO, writingSince int64, timeout time.Duration) string {
	if writingSince != 0 && now.Sub(time.Unix(0, writingSince)) >= timeout {
		return fmt.Sprintf("adb server accepted no data for %s (%s)", timeout, service)
	}
	if isInstallWrite(service) && now.Sub(time.Unix(0, lastIO)) >= timeout {
		return fmt.Sprintf("install stream moved no data for %s (%s)", timeout, service)
	}
	return ""
}

func isInstallWrite(service string) bool {
	return strings.HasPrefix(service, "exec:cmd package install-write") ||
		strings.HasPrefix(service, "exec:pm install-write") ||
		strings.HasPrefix(service, "abb_exec:package\x00install-write")
}
