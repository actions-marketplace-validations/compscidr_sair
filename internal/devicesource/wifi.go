package devicesource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
			if _, err = s.shell(ctx, req.Serial, "svc wifi disable"); err == nil {
				if _, err = s.shell(ctx, req.Serial, "svc wifi enable"); err == nil {
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
	return s.waitFor(ctx, s.wifiTimeout, fmt.Sprintf("%s not online on Wi-Fi %q", serial, s.wifiSSID), func() error {
		return s.checkOnline(ctx, serial)
	})
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
		return errors.New("not associated to any network")
	case st.ssid != s.wifiSSID:
		return fmt.Errorf("associated to %q, not %q", st.ssid, s.wifiSSID)
	}
	out, err := s.shell(ctx, serial, "dumpsys connectivity")
	if err != nil {
		return fmt.Errorf("dumpsys connectivity: %w", err)
	}
	if err := wifiValidatedNotCellularDefault(out); err != nil {
		return err
	}
	if _, err := s.shell(ctx, serial, "ping -c1 -W2 "+shellQuote(s.wifiPingHost)); err != nil {
		return fmt.Errorf("cannot reach %s (DNS or route)", s.wifiPingHost)
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
	out, err := s.shell(ctx, serial, "cmd wifi status")
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
		return errors.New("connected but not VALIDATED")
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
// shell protocol). Errors carry the output but never the command.
func (s *Server) adbShell(ctx context.Context, serial, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "adb", "-P", strconv.Itoa(s.adbPort), "-s", serial, "shell", "sh")
	cmd.Stdin = strings.NewReader(command + "\n")
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
