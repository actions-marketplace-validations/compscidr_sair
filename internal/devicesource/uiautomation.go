package devicesource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// uiaProbe connects a UiAutomation and lets it go, the way an
// instrumentation's runner does on start. A leaked registration fails it with
// "UiAutomationService ... already registered!". Android before 4.3 has no
// UiAutomation and never prints that, so the probe is a no-op there.
const uiaProbe = "uiautomator dump /data/local/tmp/sair-uia-probe.xml 2>&1; rm -f /data/local/tmp/sair-uia-probe.xml"

// ClearUiAutomation (sair#128) finds a UiAutomation registration a dead
// session leaked (a killed `am instrument`, a uiautomator server left
// running) and clears it without a reboot when it can: the registration is
// tied to its holder's binder, so killing the shell-launched process that
// holds it unregisters it. Reboots only when the probe still fails after that.
func (s *Server) ClearUiAutomation(ctx context.Context, req *pb.UiAutomationRequest) (*pb.UiAutomationResponse, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	leaked, err := s.uiaLeaked(ctx, req.Serial)
	if err != nil || !leaked {
		return &pb.UiAutomationResponse{}, err
	}
	ps, _ := s.shell(ctx, req.Serial, "ps -A")
	if strings.Count(strings.TrimSpace(ps), "\n") == 0 {
		ps, _ = s.shell(ctx, req.Serial, "ps") // toolbox ps (Android < 8) reads -A as a name filter
	}
	var killed []string
	for _, h := range uiaHolders(ps) {
		pid, _, _ := strings.Cut(h, " ")
		if _, err := s.shell(ctx, req.Serial, "kill -9 "+pid); err == nil {
			killed = append(killed, h)
		}
	}
	slog.Warn("leaked UiAutomation registration; killed the shell processes that can hold one", "serial", req.Serial, "killed", killed)
	if len(killed) > 0 {
		// Binder death reaches system_server asynchronously.
		err := s.waitFor(ctx, 10*time.Second, "UiAutomation still registered", func() error {
			if leaked, err := s.uiaLeaked(ctx, req.Serial); err != nil || leaked {
				return errors.Join(err, errors.New("probe still says already registered"))
			}
			return nil
		})
		if err == nil {
			return &pb.UiAutomationResponse{Cleared: "killed " + strings.Join(killed, ", ")}, nil
		}
	}
	slog.Warn("UiAutomation leak survived killing its holders; rebooting", "serial", req.Serial)
	if err := s.reboot(ctx, req.Serial); errors.Is(err, errUnsafeReboot) {
		return nil, status.Errorf(codes.FailedPrecondition, "%s: leaked UiAutomation registration, not cleared by killing its holders, and %v", req.Serial, err)
	} else if err != nil {
		return nil, err
	}
	if leaked, err := s.uiaLeaked(ctx, req.Serial); err != nil || leaked {
		return nil, status.Errorf(codes.Internal, "%s: UiAutomation still registered after a reboot: %v", req.Serial, err)
	}
	return &pb.UiAutomationResponse{Cleared: "rebooted"}, nil
}

func (s *Server) uiaLeaked(ctx context.Context, serial string) (bool, error) {
	out, err := s.shell(ctx, serial, uiaProbe)
	if err != nil {
		return false, status.Errorf(codes.Internal, "%s: uiautomator probe: %v", serial, err)
	}
	return strings.Contains(out, "already registered"), nil
}

// uiaHolders reads `ps` (toybox or toolbox: USER and PID first, NAME last)
// and returns "<pid> <name>" for each shell-user process that can hold a
// UiAutomation: an app_process tool (`am instrument` keeps the connection in
// its own process; monkey and uiautomator are tools too) or a uiautomator
// server. The device is unacquired, so whatever is left is a dead job's.
//
// ponytail: on Android < 7, where pm is an app_process tool, this also kills
// a device source `pm` call racing the reset, failing that call once.
func uiaHolders(ps string) []string {
	var holders []string
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "shell" {
			continue
		}
		name := path.Base(f[len(f)-1])
		if strings.HasPrefix(name, "app_process") || strings.HasPrefix(name, "uiautomator") || strings.HasPrefix(name, "com.android.commands.") {
			holders = append(holders, fmt.Sprintf("%s %s", f[1], name))
		}
	}
	return holders
}
