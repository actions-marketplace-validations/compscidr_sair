package devicesource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// SetWifi puts a device's Wi-Fi in a known state using the bench network from
// WIFI_SSID / WIFI_PASSPHRASE. Only suggestions added by the shell uid are
// touched: a test app's own suggestions are its own to clean up.
func (s *Server) SetWifi(ctx context.Context, req *pb.WifiRequest) (*emptypb.Empty, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	if s.wifiSSID == "" {
		return nil, status.Error(codes.FailedPrecondition, "WIFI_SSID is not set on the device source; Wi-Fi is not managed on this bench")
	}
	slog.Info("SetWifi", "serial", req.Serial, "state", req.State)

	var err error
	switch req.State {
	case pb.WifiRequest_RESET:
		// Radio toggled and back on, not associated, no Internet: the next
		// lock starts from that, not from "it will be shortly" (sair#86).
		if err = s.wifiForget(ctx, req.Serial); err == nil {
			if err = s.svc(ctx, req.Serial, "wifi disable"); err == nil {
				if err = s.svc(ctx, req.Serial, "wifi enable"); err == nil {
					err = s.waitFor(ctx, s.wifiOffTimeout, req.Serial+" not back to radio on and offline", func() error {
						return s.checkOffline(ctx, req.Serial, true)
					})
				}
			}
		}
	case pb.WifiRequest_OFF:
		if err = s.wifiForget(ctx, req.Serial); err == nil {
			err = s.waitFor(ctx, s.wifiOffTimeout, req.Serial+" not offline", func() error {
				return s.checkOffline(ctx, req.Serial, false)
			})
		}
	case pb.WifiRequest_ON:
		err = s.wifiConnect(ctx, req.Serial)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown Wi-Fi state %d", req.State)
	}
	if err != nil {
		if _, ok := status.FromError(err); !ok {
			err = status.Errorf(codes.Internal, "%s: Wi-Fi %s: %v", req.Serial, req.State, err)
		}
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// wifiForget drops every shell-uid suggestion and saved network. If the shell
// may not remove all suggestions it falls back to the bench SSID alone.
func (s *Server) wifiForget(ctx context.Context, serial string) error {
	if _, err := s.shell(ctx, serial, "cmd wifi remove-all-suggestions"); err != nil {
		slog.Warn("remove-all-suggestions failed; removing the bench SSID only", "serial", serial, "error", err)
		if _, err := s.shell(ctx, serial, "cmd wifi remove-suggestion "+shellQuote(s.wifiSSID)); err != nil {
			return fmt.Errorf("remove suggestion: %w", err)
		}
	}
	out, err := s.shell(ctx, serial, "cmd wifi list-networks")
	if err != nil {
		slog.Warn("list-networks failed; saved networks not forgotten", "serial", serial, "error", err)
		return nil
	}
	for _, id := range savedNetworkIDs(out) {
		if _, err := s.shell(ctx, serial, "cmd wifi forget-network "+id); err != nil {
			return fmt.Errorf("forget network %s: %w", id, err)
		}
	}
	return nil
}

// wifiConnect suggests the bench network and waits until the device is
// online on it (see checkOnline), so a job never gets a phone that is still
// associating, or one that answers a ping over some other network.
func (s *Server) wifiConnect(ctx context.Context, serial string) error {
	add := "cmd wifi add-suggestion " + shellQuote(s.wifiSSID) + " open"
	if s.wifiPassphrase != "" {
		add = "cmd wifi add-suggestion " + shellQuote(s.wifiSSID) + " wpa2 " + shellQuote(s.wifiPassphrase)
	}
	if _, err := s.shell(ctx, serial, add); err != nil {
		return fmt.Errorf("add suggestion: %w", err)
	}
	if _, err := s.shell(ctx, serial, "cmd wifi start-scan"); err != nil {
		return fmt.Errorf("start scan: %w", err)
	}
	start := time.Now()
	var last error
	err := s.waitFor(ctx, s.wifiTimeout, fmt.Sprintf("%s not online on Wi-Fi %q", serial, s.wifiSSID), func() error {
		last = s.checkOnline(ctx, serial)
		return last
	})
	if ctx.Err() == nil { // a cancelled job says nothing about the phone
		s.recordJoin(serial, time.Since(start), err, last)
	}
	return err
}

// wifiStats is one phone's Wi-Fi ON outcomes since the device source started
// (sair#96), reported as Device.wifi.
type wifiStats struct {
	joins, failures int32
	byReason        map[string]int32
	lastFailure     string
	lastFailureAt   time.Time
	recent          []time.Duration // time to online of the latest 100 successes, oldest first
	slowest         time.Duration
	warned          bool // over the failure rate and already logged
}

// recordJoin counts one ON attempt; err is its outcome and last the final
// reason checkOnline gave. A phone that fails one join in ten (after five)
// is logged once, then again only after it has dropped below and come back.
func (s *Server) recordJoin(serial string, took time.Duration, err, last error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.wifi == nil {
		s.wifi = make(map[string]*wifiStats)
	}
	st := s.wifi[serial]
	if st == nil {
		st = &wifiStats{byReason: make(map[string]int32)}
		s.wifi[serial] = st
	}
	st.joins++
	if err == nil {
		st.recent = append(st.recent, took)
		if len(st.recent) > 100 {
			st.recent = st.recent[1:]
		}
		st.slowest = max(st.slowest, took)
	} else {
		st.failures++
		st.byReason[joinFailureReason(last)]++
		st.lastFailure = status.Convert(err).Message()
		st.lastFailureAt = time.Now()
	}
	bad := st.joins >= 5 && st.failures*10 >= st.joins
	if bad && !st.warned {
		slog.Warn("phone often fails to get online on the bench Wi-Fi", "serial", serial, "failures", st.failures, "joins", st.joins, "byReason", st.byReason, "hint", st.hint(s.wifiTimeout))
	}
	st.warned = bad
}

// hint is what a bench owner could do about a phone's joins: raise the
// timeout when successes come close to it, move the phone when most failures
// never associated (signal). "" when neither stands out.
func (st *wifiStats) hint(timeout time.Duration) string {
	switch {
	case st.slowest > timeout*3/4:
		return fmt.Sprintf("slowest join took %s of the %s limit: raise WIFI_CONNECT_TIMEOUT_SECONDS", st.slowest.Round(time.Second), timeout)
	case st.byReason["not_associated"]*2 > st.failures:
		return "most failed joins never associated: weak signal where the phone sits? Move it or the AP"
	}
	return ""
}

// wifiStatsReport is the phone's Device.wifi, nil before its first ON.
func (s *Server) wifiStatsReport(serial string) *pb.WifiStats {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st := s.wifi[serial]
	if st == nil {
		return nil
	}
	r := &pb.WifiStats{
		Joins:            st.joins,
		Failures:         st.failures,
		FailuresByReason: maps.Clone(st.byReason),
		LastFailure:      st.lastFailure,
		SlowestJoinMs:    st.slowest.Milliseconds(),
		TimeoutMs:        s.wifiTimeout.Milliseconds(),
		Hint:             st.hint(s.wifiTimeout),
	}
	if !st.lastFailureAt.IsZero() {
		r.LastFailureMs = st.lastFailureAt.UnixMilli()
	}
	if n := len(st.recent); n > 0 {
		r.LastJoinMs = st.recent[n-1].Milliseconds()
		r.MedianJoinMs = slices.Sorted(slices.Values(st.recent))[n/2].Milliseconds()
	}
	return r
}

// Why a phone is not online, as WifiStats.failures_by_reason counts them.
var (
	errNotAssociated = errors.New("not associated to any network")
	errOtherSSID     = errors.New("associated to")
	errNotValidated  = errors.New("connected but not VALIDATED")
	errNoRoute       = errors.New("cannot reach")
)

func joinFailureReason(err error) string {
	switch {
	case errors.Is(err, errNotAssociated):
		return "not_associated"
	case errors.Is(err, errOtherSSID):
		return "other_ssid"
	case errors.Is(err, errNotValidated):
		return "not_validated"
	case errors.Is(err, errNoRoute):
		return "no_route"
	}
	return "other"
}

// waitFor polls check every two seconds until it passes, or returns
// DeadlineExceeded after timeout with the last reason it did not.
func (s *Server) waitFor(ctx context.Context, timeout time.Duration, what string, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return status.Errorf(codes.DeadlineExceeded, "%s within %s: %v", what, timeout, err)
		}
		select {
		case <-time.After(min(2*time.Second, time.Until(deadline))):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// checkOnline is the ON contract (sair#87): associated to the bench SSID,
// that Wi-Fi is the validated default network (Android's own verdict, so the
// app under test sees it as online too), and wifiPingHost answers, which with
// a hostname also proves DNS. nil when all three hold, else why not.
func (s *Server) checkOnline(ctx context.Context, serial string) error {
	st, err := s.wifiStatus(ctx, serial)
	if err != nil {
		return err
	}
	switch {
	case !st.enabled:
		return errors.New("Wi-Fi radio is off")
	case st.ssid == "":
		return errNotAssociated
	case st.ssid != s.wifiSSID:
		return fmt.Errorf("%w %q, not %q", errOtherSSID, st.ssid, s.wifiSSID)
	}
	out, err := s.shell(ctx, serial, "dumpsys connectivity")
	if err != nil {
		return fmt.Errorf("dumpsys connectivity: %w", err)
	}
	if err := wifiValidatedNotCellularDefault(out); err != nil {
		return err
	}
	if _, err := s.shell(ctx, serial, "ping -c1 -W2 "+shellQuote(s.wifiPingHost)); err != nil {
		return fmt.Errorf("%w %s (DNS or route)", errNoRoute, s.wifiPingHost)
	}
	return nil
}

// checkOffline is the OFF contract (sair#86): not on the bench network and no
// route to wifiPingHost. Another SSID without an uplink (a test's own Wi-Fi
// Direct group, say) is offline in every sense a test cares about, and the
// ping is what catches one that does have Internet. With radioOn it also
// wants the radio back up, for RESET. A phone still on the bench SSID after
// a forget is usually re-joining through a suggestion from some app's own
// uid, which the error names so it is found from the log, not a red test.
func (s *Server) checkOffline(ctx context.Context, serial string, radioOn bool) error {
	st, err := s.wifiStatus(ctx, serial)
	if err != nil {
		return err
	}
	switch {
	case radioOn && !st.enabled:
		return errors.New("Wi-Fi radio is still off")
	case st.ssid != "" && st.ssid == s.wifiSSID:
		return fmt.Errorf("still associated to %q", st.ssid)
	}
	if _, err := s.shell(ctx, serial, "ping -c1 -W2 "+shellQuote(s.wifiPingHost)); err == nil {
		if st.ssid != "" {
			return fmt.Errorf("still reaches %s over Wi-Fi %q", s.wifiPingHost, st.ssid)
		}
		return fmt.Errorf("still reaches %s (mobile data?)", s.wifiPingHost)
	}
	return nil
}

// wifiStatusInfo is what `cmd wifi status` says about a phone. API 30+, which
// every `cmd wifi` call here already needs.
type wifiStatusInfo struct {
	enabled bool
	ssid    string // "" when not associated
}

func (s *Server) wifiStatus(ctx context.Context, serial string) (wifiStatusInfo, error) {
	out, err := s.radioShell(ctx, serial, "cmd wifi status")
	if err != nil {
		return wifiStatusInfo{}, fmt.Errorf("cmd wifi status: %w", err)
	}
	return parseWifiStatus(out), nil
}

// parseWifiStatus reads the lines that matter:
//
//	Wifi is enabled
//	Wifi is connected to "bench"      (or: Wifi is not connected)
func parseWifiStatus(out string) wifiStatusInfo {
	var st wifiStatusInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "Wifi is enabled" {
			st.enabled = true
		} else if ssid, ok := strings.CutPrefix(line, "Wifi is connected to "); ok {
			st.ssid = strings.Trim(ssid, `"`)
		}
	}
	return st
}

// wifiValidatedNotCellularDefault reads `dumpsys connectivity`: the Wi-Fi
// network must carry VALIDATED, which Android sets only after its own
// connectivity probe passed, and the active default must not be cellular
// (a SIM phone that prefers mobile data). A VPN on top is fine: it becomes
// the default with Wi-Fi underneath, and the app under test may well be
// the VPN.
//
//	Active default network: 100
//	Current Networks:
//	  NetworkAgentInfo{ ... network{100} ... nc{[ Transports: WIFI Capabilities: ...&VALIDATED&... ]} ...
func wifiValidatedNotCellularDefault(out string) error {
	lines := strings.Split(out, "\n")
	id := ""
	wifiLine := ""
	for _, line := range lines {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Active default network: "); ok {
			id = strings.TrimSpace(v)
		}
		if wifiLine == "" && slices.Contains(transports(line), "WIFI") {
			wifiLine = line
		}
	}
	if wifiLine == "" {
		return errors.New("no Wi-Fi network in dumpsys connectivity")
	}
	if !strings.Contains(wifiLine, "VALIDATED") {
		return errNotValidated
	}
	for _, line := range lines {
		if id != "" && strings.Contains(line, "network{"+id+"}") && slices.Contains(transports(line), "CELLULAR") {
			return errors.New("default network is cellular, not Wi-Fi")
		}
	}
	return nil
}

// transports parses "Transports: WIFI|VPN" out of a NetworkAgentInfo line.
func transports(line string) []string {
	_, after, ok := strings.Cut(line, "Transports: ")
	if !ok {
		return nil
	}
	return strings.Split(strings.Fields(after)[0], "|")
}

// adbShell runs one command line on the device, through `sh` reading it
// from stdin rather than as the adb service string: adbd logs every service
// it serves to logcat, where a passphrase would sit for the next job to
// read (sair#100). Exit status and output come back the same (adb's v2
// shell protocol). The script ends with an explicit exit because a
// pre-Lollipop adbd never forwards stdin's EOF, so sh would otherwise sit
// there until the context kills it (seen on Android 4.2.2: every call took
// the full timeout and the phone was never reported). Such an adbd also
// runs sh on a pty, so its output carries the echoed script and prompts;
// the parsers here key on prefixes and tokens and cope. Errors carry the
// output but never the command.
func (s *Server) adbShell(ctx context.Context, serial, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "adb", "-P", strconv.Itoa(s.adbPort), "-s", serial, "shell", "sh")
	cmd.Stdin = strings.NewReader(command + "\nexit\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// savedNetworkIDs parses `cmd wifi list-networks`:
//
//	Network Id      SSID                         Security type
//	0            bench                          wpa2-psk
func savedNetworkIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err == nil {
			ids = append(ids, f[0])
		}
	}
	return ids
}

// shellQuote quotes s for the device's sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
