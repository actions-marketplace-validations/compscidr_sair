package devicesource

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"

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
		offErr = s.waitFor(ctx, s.bluetoothTimeout, req.Serial+" Bluetooth not off", func() error {
			return s.checkBluetooth(ctx, req.Serial, false)
		})
	}
	_, onErr := s.shell(ctx, req.Serial, "svc bluetooth enable")
	if onErr == nil {
		onErr = s.waitFor(ctx, s.bluetoothTimeout, req.Serial+" Bluetooth not back on", func() error {
			return s.checkBluetooth(ctx, req.Serial, true)
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

// checkBluetooth wants the adapter ON, or with on false, off for classic
// Bluetooth: OFF, or BLE_ON on a phone that keeps BLE scanning available.
func (s *Server) checkBluetooth(ctx context.Context, serial string, on bool) error {
	st, err := s.bluetoothState(ctx, serial)
	switch {
	case err != nil:
		return err
	case on && st != "ON", !on && st != "OFF" && st != "BLE_ON":
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
