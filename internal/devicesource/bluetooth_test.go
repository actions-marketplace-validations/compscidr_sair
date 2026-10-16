package devicesource

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeRadio is a phone whose adapter takes `svc bluetooth` to off (or to
// stuck when it never comes back) and dumps it like One UI does.
func fakeRadio(s *Server, state, onGoesTo string) *[]string {
	var cmds []string
	s.shell = func(_ context.Context, _, command string) (string, error) {
		cmds = append(cmds, command)
		switch command {
		case "svc bluetooth disable":
			state = "OFF"
		case "svc bluetooth enable":
			state = onGoesTo
		case "dumpsys bluetooth_manager":
			if state == "" {
				return "", nil
			}
			return "Bluetooth Status\n  enabled: true\n  state: " + state + "\n  name: Galaxy A06\n", nil
		}
		return "", nil
	}
	return &cmds
}

func TestResetBluetoothCyclesAndWaitsForOn(t *testing.T) {
	s := &Server{bluetoothTimeout: 10 * time.Millisecond}
	cmds := fakeRadio(s, "ON", "ON")
	if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"dumpsys bluetooth_manager", "svc bluetooth disable", "dumpsys bluetooth_manager", "svc bluetooth enable", "dumpsys bluetooth_manager"}
	if !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands:\n got %q\nwant %q", *cmds, want)
	}
}

// A radio that stays off fails with the device warning set, cleared by the
// next reset that works; the screen warning is kept alongside.
func TestResetBluetoothWarnsWhenRadioStaysOff(t *testing.T) {
	s := &Server{bluetoothTimeout: 10 * time.Millisecond, warnings: map[string]string{"A": "screen locked"}}
	fakeRadio(s, "ON", "TURNING_ON")
	_, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"})
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "adapter is TURNING_ON") {
		t.Fatalf("err = %v, want DeadlineExceeded naming the state", err)
	}
	if w := s.warning("A"); !strings.Contains(w, "screen locked; ") || !strings.Contains(w, "Bluetooth not back on") {
		t.Errorf("warning = %q, want both", w)
	}
	fakeRadio(s, "OFF", "ON")
	if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); err != nil {
		t.Fatal(err)
	}
	if w := s.warning("A"); w != "screen locked" {
		t.Errorf("warning = %q, want only the screen one", w)
	}
}

// Android 4.2.2 has no bluetooth_manager dump: skipped, nothing toggled.
func TestResetBluetoothUnimplementedWithoutAdapterState(t *testing.T) {
	s := &Server{bluetoothTimeout: 10 * time.Millisecond}
	cmds := fakeRadio(s, "", "ON")
	if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented", err)
	}
	if len(*cmds) != 1 || s.warning("A") != "" {
		t.Errorf("commands %q, warning %q: want one dump and no warning", *cmds, s.warning("A"))
	}
}
