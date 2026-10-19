package devicesource

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeRadio is a phone whose adapter follows `svc bluetooth` and dumps it
// like One UI does. disable stops at BLE_ON while BLE scanning is on with
// scanApps registered (the setting drops them) or while stickyApps are
// registered (only a force-stop drops them); enable goes to onGoesTo.
type fakeRadio struct {
	state, onGoesTo      string
	scan                 bool
	scanApps, stickyApps []string
	cmds                 []string
}

func (f *fakeRadio) install(s *Server) {
	s.shell = func(_ context.Context, _, command string) (string, error) {
		f.cmds = append(f.cmds, command)
		apps := slices.Concat(f.stickyApps)
		if f.scan {
			apps = slices.Concat(f.scanApps, f.stickyApps)
		}
		settle := func() {
			if f.state == "BLE_ON" && len(apps) == 0 {
				f.state = "OFF"
			}
		}
		switch {
		case command == "svc bluetooth disable":
			f.state = "BLE_ON"
			settle()
		case command == "svc bluetooth enable":
			f.state = f.onGoesTo
		case command == "settings get global ble_scan_always_enabled":
			if f.scan {
				return "1\n", nil
			}
			return "0\n", nil
		case strings.HasPrefix(command, "settings put global ble_scan_always_enabled "):
			f.scan = strings.HasSuffix(command, " 1")
			if !f.scan {
				apps = f.stickyApps
			}
			settle()
		case strings.HasPrefix(command, "am force-stop "):
			pkg := strings.Trim(strings.TrimPrefix(command, "am force-stop "), "'")
			f.stickyApps = slices.DeleteFunc(f.stickyApps, func(a string) bool { return a == pkg })
			f.scanApps = slices.DeleteFunc(f.scanApps, func(a string) bool { return a == pkg })
			apps = slices.DeleteFunc(apps, func(a string) bool { return a == pkg })
			settle()
		case command == "dumpsys bluetooth_manager":
			if f.state == "" {
				return "", nil
			}
			return fmt.Sprintf("Bluetooth Status\n  enabled: true\n  state: %s\n  name: Galaxy A06\n\n%d BLE apps registered\n\n%d Samsung BLE apps registered\n  %s\n\nBluetoothManagerService:\n  mEnable:true\n",
				f.state, len(apps), len(f.stickyApps), strings.Join(apps, "\n  ")), nil
		}
		return "", nil
	}
}

func TestResetBluetoothCyclesAndWaitsForOn(t *testing.T) {
	s := &Server{bluetoothTimeout: 10 * time.Millisecond}
	f := &fakeRadio{state: "ON", onGoesTo: "ON"}
	f.install(s)
	if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"dumpsys bluetooth_manager", "svc bluetooth disable", "dumpsys bluetooth_manager", "dumpsys bluetooth_manager", "svc bluetooth enable", "dumpsys bluetooth_manager"}
	if !reflect.DeepEqual(f.cmds, want) {
		t.Errorf("commands:\n got %q\nwant %q", f.cmds, want)
	}
}

// BLE_ON is not off: BLE scanning is turned off for the cycle (enough on
// the A16), then the apps still holding BLE are force-stopped (the A06's
// Samsung ones), never the one the setting already dropped. Scanning is
// back on before Bluetooth is.
func TestResetBluetoothTakesBLEDownToo(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sticky, stop []string
	}{
		{"scan setting is enough", nil, nil},
		{"samsung apps need a force-stop", []string{"com.samsung.android.mdx.kit", "com.samsung.android.beaconmanager"},
			[]string{"am force-stop 'com.samsung.android.mdx.kit'", "am force-stop 'com.samsung.android.beaconmanager'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{bluetoothTimeout: 10 * time.Millisecond}
			f := &fakeRadio{state: "ON", onGoesTo: "ON", scan: true, scanApps: []string{"com.google.android.gms"}, stickyApps: tc.sticky}
			f.install(s)
			if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); err != nil {
				t.Fatal(err)
			}
			var stops []string
			restore, enable := -1, -1
			for i, c := range f.cmds {
				switch {
				case strings.HasPrefix(c, "am force-stop"):
					stops = append(stops, c)
				case c == "settings put global ble_scan_always_enabled 1":
					restore = i
				case c == "svc bluetooth enable":
					enable = i
				}
			}
			if !reflect.DeepEqual(stops, tc.stop) {
				t.Errorf("force-stops = %q, want %q", stops, tc.stop)
			}
			if restore < 0 || restore > enable || !f.scan {
				t.Errorf("scan setting not restored before enable: %q", f.cmds)
			}
		})
	}
}

// A radio that stays off fails with the device warning set, cleared by the
// next reset that works; the screen warning is kept alongside.
func TestResetBluetoothWarnsWhenRadioStaysOff(t *testing.T) {
	s := &Server{bluetoothTimeout: 10 * time.Millisecond, warnings: map[string]string{"A": "screen locked"}}
	(&fakeRadio{state: "ON", onGoesTo: "TURNING_ON"}).install(s)
	_, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"})
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "adapter is TURNING_ON") {
		t.Fatalf("err = %v, want DeadlineExceeded naming the state", err)
	}
	if w := s.warning("A"); !strings.Contains(w, "screen locked; ") || !strings.Contains(w, "Bluetooth not back on") {
		t.Errorf("warning = %q, want both", w)
	}
	(&fakeRadio{state: "OFF", onGoesTo: "ON"}).install(s)
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
	f := &fakeRadio{onGoesTo: "ON"}
	f.install(s)
	if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented", err)
	}
	if len(f.cmds) != 1 || s.warning("A") != "" {
		t.Errorf("commands %q, warning %q: want one dump and no warning", f.cmds, s.warning("A"))
	}
}

// Real dumps at BLE_ON: One UI (cube A06, Android 14) and AOSP (Moto g15
// and Redmi Note 14, Android 15), with the flag dump that follows on 15.
func TestBleApps(t *testing.T) {
	oneUI := "Bluetooth crashed 0 times\n\n3 BLE apps registered\n\n2 Samsung BLE apps registered\n  com.google.android.gms\n  com.samsung.android.mdx.kit\n  com.samsung.android.beaconmanager\n\nBluetoothManagerService:\n  mEnable:true\n"
	aosp := "Bluetooth crashed 0 times\n\n1 BLE app registered\n  com.google.android.gms\n\nBluetoothManagerService:\n  mEnable:false\n\n🚩Flag dump:\n\t[■]: bleContextMapRemoveFix                                 ble_context_map_remove_fix\n"
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"one ui", oneUI, []string{"com.google.android.gms", "com.samsung.android.mdx.kit", "com.samsung.android.beaconmanager"}},
		{"aosp", aosp, []string{"com.google.android.gms"}},
		{"none", "0 BLE apps registered\n\nBluetoothManagerService:\n", nil},
	} {
		if got := bleApps(tc.out); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: bleApps = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The adapter state in every dump format seen on a bench phone.
func TestBluetoothStateFormats(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"one ui / aosp", "Bluetooth Status\n  enabled: true\n  state: BLE_ON\n", "BLE_ON"},
		{"pixel 16/17", "Bluetooth Status:\n  State:         ON\n  Address:       XX:XX:XX:XX:7E:97\n\nBluetooth Activity History:\n  TIMESTAMP          ACTION/STATE     DETAILS\n", "ON"},
		{"android 4.x", "", ""},
	} {
		s := &Server{shell: func(context.Context, string, string) (string, error) { return tc.out, nil }}
		if got, err := s.bluetoothState(context.Background(), "A"); err != nil || got != tc.want {
			t.Errorf("%s: state = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// A phone that hangs on svc with a healthy radio (Galaxy A13) still
// resets: the bounded svc gives up and the state wait decides (sair#139).
func TestResetBluetoothGoesByStateWhenSvcHangs(t *testing.T) {
	s := &Server{bluetoothTimeout: 10 * time.Millisecond, radioTimeout: 10 * time.Millisecond}
	f := &fakeRadio{state: "ON", onGoesTo: "ON"}
	f.install(s)
	radio := s.shell
	s.shell = func(ctx context.Context, serial, command string) (string, error) {
		out, err := radio(ctx, serial, command)
		if strings.HasPrefix(command, "svc ") {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return out, err
	}
	if _, err := s.ResetBluetooth(context.Background(), &pb.ResetBluetoothRequest{Serial: "A"}); err != nil {
		t.Fatal(err)
	}
}
