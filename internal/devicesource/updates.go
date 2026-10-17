package devicesource

import (
	"context"
	"strconv"
	"strings"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const playStore = "com.android.vending"

// HoldUpdates (sair#151) disables the Play Store for a lease: when it
// replaces com.google.android.gms, Android force-stops every app that
// depends on it, the app under instrumentation included, and the job fails
// with "Process crashed". Released, it is enabled again and updates install
// between leases.
func (s *Server) HoldUpdates(ctx context.Context, req *pb.HoldUpdatesRequest) (*emptypb.Empty, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	// Android 4.2's pm crashes on disable-user and prints a whole bugreport.
	if sdk, _ := strconv.Atoi(s.buildProps(req.Serial).sdk); sdk < 21 {
		return nil, status.Errorf(codes.Unimplemented, "%s: Android SDK %d, pm cannot disable packages", req.Serial, sdk)
	}
	// pm disable-user on a missing package also crashes on some phones.
	if out, err := s.shell(ctx, req.Serial, "pm path "+playStore); err != nil || !strings.Contains(out, "package:") {
		return nil, status.Errorf(codes.Unimplemented, "%s: no Play Store", req.Serial)
	}
	cmd := "pm enable " + playStore
	if req.Hold {
		cmd = "pm disable-user " + playStore
	}
	out, err := s.shell(ctx, req.Serial, cmd)
	if strings.Contains(out, "new state:") {
		return &emptypb.Empty{}, nil
	}
	// e.g. Xiaomi: "java.lang.SecurityException: Cannot disable system packages."
	msg := strings.TrimSpace(out)
	for line := range strings.Lines(out) {
		if strings.Contains(line, "Exception:") {
			msg = strings.TrimSpace(line)
			break
		}
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return nil, status.Errorf(codes.FailedPrecondition, "%s: %s: %s", req.Serial, cmd, msg)
}
