package devicesource

import (
	"context"
	"reflect"
	"testing"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const adbDevices = "List of devices attached\nCI1\tdevice\nDEV1\tdevice\nCI2\tdevice\nUNAUTH\tunauthorized\n\n"

// DEVICE_SERIALS keeps only the listed phones, EXCLUDE_SERIALS drops the
// listed ones, neither reports everything in state "device" (sair#90).
func TestSerialFilter(t *testing.T) {
	tests := []struct {
		name        string
		allow, deny []string
		want        []string
	}{
		{"unfiltered", nil, nil, []string{"CI1", "DEV1", "CI2"}},
		{"allowlist", []string{"CI1", "CI2", "TYPO"}, nil, []string{"CI1", "CI2"}},
		{"allowlist set but empty", []string{}, nil, nil},
		{"denylist", nil, []string{"DEV1"}, []string{"CI1", "CI2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{allow: packageSet(tt.allow), deny: packageSet(tt.deny)}
			if tt.allow != nil && s.allow == nil {
				s.allow = map[string]struct{}{}
			}
			got := s.filterSerials(parseAdbDevices(adbDevices))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A filtered phone is as good as unplugged: every per-device RPC refuses it
// before touching the phone, so no lock, reset or forward can reach it.
func TestFilteredSerialIsRefusedByRPCs(t *testing.T) {
	s := &Server{wifiSSID: "bench", deny: packageSet([]string{"DEV1"}), baselineOverride: map[string]struct{}{}}
	cmds := fakeShell(s, nil)
	if _, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "DEV1", State: pb.WifiRequest_RESET}); status.Code(err) != codes.NotFound {
		t.Errorf("SetWifi: err = %v, want NotFound", err)
	}
	if _, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "DEV1"}); status.Code(err) != codes.NotFound {
		t.Errorf("ResetPackages: err = %v, want NotFound", err)
	}
	if len(*cmds) != 0 {
		t.Errorf("a filtered phone was touched: %q", *cmds)
	}
}
