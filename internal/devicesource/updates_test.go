package devicesource

import (
	"context"
	"reflect"
	"strings"
	"testing"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Outputs as the phones printed them (sair#151): Moto g15 (Android 15) takes
// the change, the Redmi Note 14 refuses it, and the Galaxy SM-G3502 (4.2.2)
// crashes pm into a bugreport, so it is never asked.
func TestHoldUpdates(t *testing.T) {
	const path = "pm path com.android.vending"
	for _, tc := range []struct {
		name, sdk string
		hold      bool
		outputs   map[string]string
		code      codes.Code
		msg       string
		cmds      []string
	}{
		{"hold", "35", true, map[string]string{path: "package:/product/priv-app/Phonesky/Phonesky.apk\n", "pm disable-user com.android.vending": "Package com.android.vending new state: disabled-user\n"},
			codes.OK, "", []string{path, "pm disable-user com.android.vending"}},
		{"resume", "35", false, map[string]string{path: "package:/product/priv-app/Phonesky/Phonesky.apk\n", "pm enable com.android.vending": "Package com.android.vending new state: enabled\n"},
			codes.OK, "", []string{path, "pm enable com.android.vending"}},
		{"xiaomi refuses", "35", true, map[string]string{path: "package:/product/app/Phonesky/Phonesky.apk\n", "pm disable-user com.android.vending": "\nException occurred while executing 'disable-user':\njava.lang.SecurityException: Cannot disable system packages.\n\tat com.android.server.pm.PackageManagerServiceImpl.shouldRestrictEnabledSettingsChange(PackageManagerServiceImpl.java:893)\n"},
			codes.FailedPrecondition, "java.lang.SecurityException: Cannot disable system packages.", []string{path, "pm disable-user com.android.vending"}},
		{"no play store", "35", true, map[string]string{}, codes.Unimplemented, "no Play Store", []string{path}},
		{"android 4.2", "17", true, map[string]string{path: "package:/system/app/Play.apk\n"}, codes.Unimplemented, "SDK 17", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{props: map[string]buildProps{"A": {sdk: tc.sdk}}}
			cmds := fakeShell(s, tc.outputs)
			_, err := s.HoldUpdates(context.Background(), &pb.HoldUpdatesRequest{Serial: "A", Hold: tc.hold})
			if status.Code(err) != tc.code || !strings.Contains(status.Convert(err).Message(), tc.msg) {
				t.Errorf("err = %v, want %v containing %q", err, tc.code, tc.msg)
			}
			if !reflect.DeepEqual(*cmds, tc.cmds) {
				t.Errorf("commands = %q, want %q", *cmds, tc.cmds)
			}
		})
	}
}
