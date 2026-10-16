package devicesource

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ResetBluetooth cycles the radio off and back on and waits until the
// adapter reports ON (sair#127): a stack an earlier job wedged (stuck
// advertisers, a scanner that never restarted) otherwise shows up in the
// next job as "No peers discovered". Bluetooth is turned back on even when
// it never reported off, since that is what tests expect. The outcome is
// the device's Bluetooth warning, so a radio that stays off shows on the
// dashboard, not only in a BLE test.
func (s *Server) ResetBluetooth(ctx context.Context, req *pb.ResetBluetoothRequest) (*emptypb.Empty, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	st, err := s.bluetoothState(ctx, req.Serial)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: Bluetooth: %v", req.Serial, err)
	}
	if st == "" {
		return nil, status.Errorf(codes.Unimplemented, "%s: no adapter state in dumpsys bluetooth_manager; Bluetooth is not managed on this phone", req.Serial)
	}
	slog.Info("ResetBluetooth", "serial", req.Serial, "state", st)

	_, offErr := s.shell(ctx, req.Serial, "svc bluetooth disable")
	if offErr == nil {
		offErr = s.bluetoothOff(ctx, req.Serial)
	}
	_, onErr := s.shell(ctx, req.Serial, "svc bluetooth enable")
	if onErr == nil {
		onErr = s.waitFor(ctx, s.bluetoothTimeout, req.Serial+" Bluetooth not back on", func() error {
			return s.checkBluetooth(ctx, req.Serial, "ON")
		})
	}
	err = cmp.Or(onErr, offErr)
	if err != nil {
		if _, ok := status.FromError(err); !ok {
			err = status.Errorf(codes.Internal, "%s: Bluetooth reset: %v", req.Serial, err)
		}
	}
	s.stateMu.Lock()
	if s.btWarnings == nil {
		s.btWarnings = make(map[string]string)
	}
	if err != nil {
		s.btWarnings[req.Serial] = status.Convert(err).Message()
	} else {
		delete(s.btWarnings, req.Serial)
	}
	s.stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// bluetoothOff waits, after `svc bluetooth disable`, until the adapter is
// fully OFF. Disable alone stops at BLE_ON on a phone with "Nearby device
// scanning" on or an app holding BLE through enableBle, and BLE is where a
// wedged advertiser or scanner lives. So BLE scanning is turned off, which
// drops the AOSP-tracked BLE apps (all the A16 had), and whatever the dump
// still lists as a BLE app is force-stopped (the A06's One UI beaconmanager
// and mdx.kit ignore the setting). The setting is put back on return, before
// Bluetooth is enabled again. Seen on the cube's phones, Oct 2026.
func (s *Server) bluetoothOff(ctx context.Context, serial string) error {
	wait := func(timeout time.Duration, states ...string) error {
		return s.waitFor(ctx, timeout, serial+" Bluetooth not off", func() error {
			return s.checkBluetooth(ctx, serial, states...)
		})
	}
	if err := wait(s.bluetoothTimeout, "OFF", "BLE_ON"); err != nil {
		return err
	}
	if s.checkBluetooth(ctx, serial, "OFF") == nil {
		return nil
	}
	if scan, err := s.shell(ctx, serial, "settings get global ble_scan_always_enabled"); err == nil && strings.TrimSpace(scan) == "1" {
		if _, err := s.shell(ctx, serial, "settings put global ble_scan_always_enabled 0"); err == nil {
			defer s.shell(context.WithoutCancel(ctx), serial, "settings put global ble_scan_always_enabled 1")
			if wait(min(5*time.Second, s.bluetoothTimeout), "OFF") == nil {
				return nil
			}
		}
	}
	out, err := s.shell(ctx, serial, "dumpsys bluetooth_manager")
	if err != nil {
		return fmt.Errorf("dumpsys bluetooth_manager: %w", err)
	}
	apps := bleApps(out)
	slog.Info("Bluetooth held at BLE_ON; force-stopping its BLE apps", "serial", serial, "apps", apps)
	for _, pkg := range apps {
		if _, err := s.shell(ctx, serial, "am force-stop "+shellQuote(pkg)); err != nil {
			slog.Warn("force-stop failed", "serial", serial, "package", pkg, "error", err)
		}
	}
	if err := wait(s.bluetoothTimeout, "OFF"); err != nil {
		return fmt.Errorf("%w (BLE apps: %s)", err, cmp.Or(strings.Join(apps, ", "), "none listed"))
	}
	return nil
}

// bleApps lists the packages under the dump's "BLE apps registered" headers:
//
//	3 BLE apps registered
//
//	2 Samsung BLE apps registered
//	  com.google.android.gms
//	  com.samsung.android.beaconmanager
func bleApps(out string) []string {
	var apps []string
	inList := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.Contains(strings.ToLower(t), "ble app"):
			inList = true
		case inList && t != "" && line != t && packageName.MatchString(t):
			apps = append(apps, t)
		case t != "":
			inList = false
		}
	}
	return apps
}

var packageName = regexp.MustCompile(`^[A-Za-z]\w*(\.\w+)+$`)

// checkBluetooth wants the adapter in one of states.
func (s *Server) checkBluetooth(ctx context.Context, serial string, states ...string) error {
	st, err := s.bluetoothState(ctx, serial)
	if err != nil {
		return err
	}
	if !slices.Contains(states, st) {
		return fmt.Errorf("adapter is %s", cmp.Or(st, "unknown"))
	}
	return nil
}

// bluetoothState is the adapter state `dumpsys bluetooth_manager` gives
// first, "" when it gives none (Android 4.x has no such service):
//
//	Bluetooth Status
//	  enabled: true
//	  state: ON                (One UI, Android 14 and 16)
//
// A phone without the service at all ("Can't find service") is "" too.
func (s *Server) bluetoothState(ctx context.Context, serial string) (string, error) {
	out, err := s.shell(ctx, serial, "dumpsys bluetooth_manager")
	if err != nil && strings.Contains(out, "Can't find service") {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("dumpsys bluetooth_manager: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "state:"); ok {
			return strings.TrimPrefix(strings.TrimSpace(v), "STATE_"), nil
		}
	}
	return "", nil
}
