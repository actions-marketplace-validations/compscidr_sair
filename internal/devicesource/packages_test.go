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

const (
	// The last job's app and test package are still on the phone when the
	// device source first sees it; pm list instrumentation names both.
	basePackages    = "package:com.bench.keep\npackage:com.example.app.test\npackage:com.vendor.tool\npackage:com.example.app\n"
	instrumentation = "instrumentation:com.example.app.test/androidx.test.runner.AndroidJUnitRunner (target=com.example.app)\n"
	jobPackages     = "package:com.bench.keep\npackage:com.example.app.test\npackage:com.vendor.tool\npackage:com.example.app\npackage:com.stubborn\n"
	suggestions     = "SSID                             Security type(s)\nbench                            wpa2-psk\nDIRECT-TEST-ab                   wpa2-psk\n"
)

// The baseline is read once per phone and reused, minus the instrumentation
// packages a previous job left; a release then removes exactly what is not
// baseline, in order, reports it with the Wi-Fi suggestions that went, and
// keeps going past a package that will not go.
func TestResetPackagesRemovesWhatTheJobAdded(t *testing.T) {
	s := &Server{stopPackages: []string{"com.android.chrome"}, clearPackages: []string{"com.android.chrome"}}
	outputs := map[string]string{
		"pm list packages -3":                 basePackages,
		"pm list instrumentation":             instrumentation,
		"pm uninstall 'com.example.app'":      "Success\n",
		"pm uninstall 'com.example.app.test'": "Success\n",
		"pm uninstall 'com.stubborn'":         "Failure [DELETE_FAILED_DEVICE_POLICY_MANAGER]\n",
		"cmd wifi list-all-suggestions":       suggestions,
	}
	cmds := fakeShell(s, outputs)
	s.recordBaseline("ABC")
	s.recordBaseline("ABC")
	if got := len(*cmds); got != 2 {
		t.Fatalf("baseline read %d commands, want 2 (packages, instrumentation): %q", got, *cmds)
	}

	// The job installed its app and test package; one more refuses to go.
	outputs["pm list packages -3"] = jobPackages
	after := strings.Replace(suggestions, "bench                            wpa2-psk\n", "", 1)
	calls := 0
	inner := s.shell
	s.shell = func(ctx context.Context, serial, command string) (string, error) {
		out, err := inner(ctx, serial, command)
		if command == "cmd wifi list-all-suggestions" {
			if calls++; calls == 2 {
				return after, nil
			}
		}
		return out, err
	}
	resp, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"com.example.app", "com.example.app.test"}; !reflect.DeepEqual(resp.Removed, want) {
		t.Errorf("removed = %v, want %v", resp.Removed, want)
	}
	if want := []string{"bench                            wpa2-psk"}; !reflect.DeepEqual(resp.SuggestionsRemoved, want) {
		t.Errorf("suggestions removed = %q, want %q", resp.SuggestionsRemoved, want)
	}
	want := []string{
		"pm list packages -3", "pm list instrumentation", // the baseline
		"cmd wifi list-all-suggestions", "pm list packages -3",
		"pm uninstall 'com.example.app'", "pm uninstall 'com.example.app.test'", "pm uninstall 'com.stubborn'",
		"am force-stop 'com.android.chrome'", "pm clear 'com.android.chrome'",
		"cmd wifi list-all-suggestions",
	}
	if !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands:\n got %q\nwant %q", *cmds, want)
	}
}

func TestResetPackagesNeedsABaseline(t *testing.T) {
	s := &Server{}
	cmds := fakeShell(s, nil)
	_, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC"})
	if status.Code(err) != codes.FailedPrecondition || len(*cmds) != 0 {
		t.Fatalf("err = %v, cmds = %q; want FailedPrecondition and no commands", err, *cmds)
	}
}

// BASELINE_PACKAGES replaces the recorded set for every phone; nothing is
// read on first sight, and only the listed packages survive a release.
func TestResetPackagesHonoursTheOverride(t *testing.T) {
	s := &Server{baselineOverride: packageSet(packageList(" com.bench.keep ,com.vendor.tool"))}
	cmds := fakeShell(s, map[string]string{
		"pm list packages -3":                 jobPackages,
		"pm uninstall 'com.example.app'":      "Success",
		"pm uninstall 'com.example.app.test'": "Success",
		"pm uninstall 'com.stubborn'":         "Success",
	})
	s.recordBaseline("ABC")
	if len(*cmds) != 0 {
		t.Fatalf("baseline read despite the override: %q", *cmds)
	}
	resp, err := s.ResetPackages(context.Background(), &pb.ResetPackagesRequest{Serial: "ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"com.example.app", "com.example.app.test", "com.stubborn"}; !reflect.DeepEqual(resp.Removed, want) {
		t.Errorf("removed = %v, want %v", resp.Removed, want)
	}
	if resp.SuggestionsRemoved != nil {
		t.Errorf("no suggestion listing, yet reported %q", resp.SuggestionsRemoved)
	}
}

func TestInstrumentationPackages(t *testing.T) {
	out := "instrumentation:com.example.app.test/androidx.test.runner.AndroidJUnitRunner (target=com.example.app)\n" +
		"instrumentation:com.other.tests/.Runner (target=com.other)\n"
	want := []string{"com.example.app", "com.example.app.test", "com.other", "com.other.tests"}
	if got := instrumentationPackages(out); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
