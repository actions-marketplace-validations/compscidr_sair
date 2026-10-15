package proxy

import (
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/compscidr/sair/internal/logging"
	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A granted lock whose phone cannot join the Wi-Fi sends a WIFI_ON event
// with the cause and an ACQUIRE wifi_failed event, and the released lock's
// log carries both processes' lines about that phone, not other phones'
// (sair#97).
func TestAcquireWifiFailureSendsEventsAndExcerpt(t *testing.T) {
	logging.KeepRecent()
	fake := &fakeOrchClient{resp: &pb.AcquireLockResponse{LockId: "L1", Serials: []string{"A"}}}
	m := NewScopedPortManager(&CommandRouter{orchClient: fake, apiKey: "k"}, nil, 3600)
	var mu sync.Mutex
	var events []*pb.BenchEvent
	m.event = func(e *pb.BenchEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
	m.recentSourceLog = func(serial string) ([]string, error) { return []string{"ds SetWifi serial=" + serial}, nil }
	m.setWifi = func(serial string, state dspb.WifiRequest_State) error {
		if state == dspb.WifiRequest_ON {
			slog.Info("joining", "serial", serial)
			slog.Info("unrelated", "serial", "OTHER")
			return status.Error(codes.DeadlineExceeded, `A not online on Wi-Fi "bench" within 1m0s: not associated to any network`)
		}
		return nil
	}

	if _, err := m.Acquire(nil, 0, "", "", 0, true, false, ""); err == nil {
		t.Fatal("acquire succeeded with a phone off the Wi-Fi")
	}

	got := map[pb.BenchEventKind]string{}
	for _, e := range events {
		if e.Serial == "A" && e.LockId == "L1" {
			got[e.Kind] = e.Outcome
		}
	}
	if got[pb.BenchEventKind_BENCH_EVENT_KIND_WIFI_ON] != "not_associated" || got[pb.BenchEventKind_BENCH_EVENT_KIND_ACQUIRE] != "wifi_failed" {
		t.Errorf("events = %v", got)
	}
	var log strings.Builder
	for _, e := range fake.lastRelease.GetLog() {
		log.WriteString(e.Service + "\n" + e.Output)
	}
	for _, want := range []string{"sair: proxy log before the failure", "joining serial=A", "sair: device source (A) log before the failure", "ds SetWifi serial=A"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("lock log is missing %q:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "OTHER") {
		t.Errorf("lock log carries another phone's lines:\n%s", log.String())
	}
}

func TestWifiOutcome(t *testing.T) {
	for msg, want := range map[string]string{
		`A not online on Wi-Fi "bench" within 1m0s: associated to "guest", not "bench"`: "other_ssid",
		`A not online on Wi-Fi "bench" within 1m0s: connected but not VALIDATED`:         "not_validated",
		`A not online on Wi-Fi "bench" within 1m0s: cannot reach 8.8.8.8 (DNS or route)`: "ping_failed",
		`A not offline within 20s: still associated to "bench"`:                          "still_associated",
		`A not offline within 20s: still reaches 8.8.8.8 (mobile data?)`:                 "still_online",
		`A not online within 1m0s: cmd wifi status: exit status 1`:                       "timeout",
	} {
		if got := wifiOutcome(status.Error(codes.DeadlineExceeded, msg)); got != want {
			t.Errorf("wifiOutcome(%q) = %q, want %q", msg, got, want)
		}
	}
	if wifiOutcome(nil) != "ok" {
		t.Error("nil is not ok")
	}
}
