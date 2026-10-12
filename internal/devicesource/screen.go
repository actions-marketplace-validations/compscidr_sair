package devicesource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Wake is the screen contract (sair#92): the phone is awake with the
// keyguard gone when this returns, or the error says why not. A one-shot
// `wm dismiss-keyguard` races on some phones (Galaxy A16, Android 16), so
// each attempt is verified and retried.
func (s *Server) Wake(ctx context.Context, req *pb.WakeRequest) (*emptypb.Empty, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	if err := s.wake(ctx, req.Serial); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// wake runs wake + dismiss and verifies, up to three times, then records the
// outcome as the device's warning (see Device.warning) so the bench owner
// sees a phone sair cannot unlock on the dashboard, not just in a red job.
func (s *Server) wake(ctx context.Context, serial string) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err = s.shell(ctx, serial, "input keyevent KEYCODE_WAKEUP"); err != nil {
			break
		}
		if _, err = s.shell(ctx, serial, "wm dismiss-keyguard"); err != nil {
			break
		}
		err = s.waitFor(ctx, s.wakeTimeout, serial+" not awake and unlocked", func() error {
			return s.checkAwake(ctx, serial)
		})
		if err == nil {
			break
		}
		slog.Warn("wake + dismiss-keyguard did not take; retrying", "serial", serial, "attempt", attempt, "error", err)
	}
	s.stateMu.Lock()
	if s.warnings == nil {
		s.warnings = make(map[string]string)
	}
	if err != nil {
		if _, ok := status.FromError(err); !ok {
			err = status.Errorf(codes.Internal, "%s: wake: %v", serial, err)
		} else if status.Code(err) == codes.DeadlineExceeded {
			err = status.Errorf(codes.FailedPrecondition, "%s: screen locked: %v. sair cannot dismiss a secure lock: on the phone set Settings > Lock screen > Screen lock type to Swipe or None", serial, status.Convert(err).Message())
		}
		s.warnings[serial] = status.Convert(err).Message()
	} else {
		delete(s.warnings, serial)
	}
	s.stateMu.Unlock()
	if err != nil {
		return err
	}
	// A bench phone that sleeps between steps re-locks itself; say so once.
	if out, err := s.shell(ctx, serial, "settings get global stay_on_while_plugged_in"); err == nil && strings.TrimSpace(out) == "0" {
		slog.Warn("stay_on_while_plugged_in is off; the phone will sleep and lock between steps. Fix: adb shell settings put global stay_on_while_plugged_in 7", "serial", serial)
	}
	return nil
}

// checkAwake reads the two lines that matter (confirmed on One UI, Android
// 14 and 16):
//
//	dumpsys power:          mWakefulness=Awake
//	dumpsys window policy:  KeyguardServiceDelegate
//	                          showing=false
func (s *Server) checkAwake(ctx context.Context, serial string) error {
	out, err := s.shell(ctx, serial, "dumpsys power")
	if err != nil {
		return fmt.Errorf("dumpsys power: %w", err)
	}
	if w := dumpsysValue(out, "mWakefulness="); w != "Awake" {
		return fmt.Errorf("screen is %s, not Awake", w)
	}
	out, err = s.shell(ctx, serial, "dumpsys window policy")
	if err != nil {
		return fmt.Errorf("dumpsys window policy: %w", err)
	}
	if v := dumpsysValue(out, "showing="); v != "false" {
		return errors.New("keyguard is showing")
	}
	return nil
}

// dumpsysValue returns what follows the first line that starts with prefix
// (trimmed), or "" when there is none.
func dumpsysValue(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// warning is the device's current Device.warning, "" when none.
func (s *Server) warning(serial string) string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.warnings[serial]
}

// lastReboot is when RebootIfStale last rebooted the device, zero if never.
func (s *Server) lastReboot(serial string) time.Time {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.reboots[serial]
}
