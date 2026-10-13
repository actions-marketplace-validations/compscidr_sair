package devicesource

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Real One UI output, Android 14 and 16 (cube bench, Oct 2026).
const (
	powerAwake     = "POWER MANAGER (dumpsys power)\n  mWakefulness=Awake\n  mWakefulnessChanging=false\n  mStayOn=true\n"
	powerAsleep    = "POWER MANAGER (dumpsys power)\n  mWakefulness=Asleep\n  mWakefulnessChanging=false\n"
	policyUnlocked = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    mKeyguardOccluded=false mKeyguardOccludedChanged=false\n    KeyguardServiceDelegate\n      showing=false\n      inputRestricted=false\n      occluded=false\n      secure=false\n      deviceHasKeyguard=true\n      KeyguardStateMonitor\n        mIsShowing=false\n"
	// Unlocked right now, but with a PIN set: a reboot would need a human.
	policySecure = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    KeyguardServiceDelegate\n      showing=false\n      inputRestricted=false\n      occluded=false\n      secure=true\n      deviceHasKeyguard=true\n"
	policyLocked = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    KeyguardServiceDelegate\n      showing=true\n      KeyguardStateMonitor\n        mIsShowing=true\n"
)

func TestWake(t *testing.T) {
	s := &Server{wakeTimeout: 10 * time.Millisecond}
	cmds := fakeShell(s, map[string]string{
		"dumpsys power":                                powerAwake,
		"dumpsys window policy":                        policyUnlocked,
		"settings get global stay_on_while_plugged_in": "15",
	})
	if _, err := s.Wake(context.Background(), &pb.WakeRequest{Serial: "A"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"input keyevent KEYCODE_WAKEUP", "wm dismiss-keyguard", "dumpsys power", "dumpsys window policy", "settings get global stay_on_while_plugged_in"}
	if !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands:\n got %q\nwant %q", *cmds, want)
	}
	if w := s.warning("A"); w != "" {
		t.Errorf("warning = %q, want none", w)
	}
}

// A keyguard that never goes (a PIN) fails after three verified attempts,
// names the fix, and leaves the device warning set until a wake succeeds.
func TestWakeFailsAndWarnsWhenKeyguardStays(t *testing.T) {
	s := &Server{wakeTimeout: 10 * time.Millisecond}
	cmds := fakeShell(s, map[string]string{
		"dumpsys power":         powerAwake,
		"dumpsys window policy": policyLocked,
	})
	_, err := s.Wake(context.Background(), &pb.WakeRequest{Serial: "A"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	for _, part := range []string{"keyguard is showing", "Screen lock type"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not mention %q", err, part)
		}
	}
	if n := countOf(*cmds, "wm dismiss-keyguard"); n != 3 {
		t.Errorf("dismiss-keyguard ran %d times, want 3", n)
	}
	if w := s.warning("A"); !strings.Contains(w, "keyguard is showing") {
		t.Errorf("warning = %q, want the keyguard reason", w)
	}

	fakeShell(s, map[string]string{"dumpsys power": powerAwake, "dumpsys window policy": policyUnlocked})
	if _, err := s.Wake(context.Background(), &pb.WakeRequest{Serial: "A"}); err != nil {
		t.Fatal(err)
	}
	if w := s.warning("A"); w != "" {
		t.Errorf("warning = %q after a successful wake, want none", w)
	}
}

// Real Android 4.2.2 output (Galaxy SM-G3502, API 17, the silverstone dogfood
// phone): no KeyguardServiceDelegate block, several fields per line.
const policyLegacy = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    mScreenOnEarly=true mScreenOnFully=true mOrientationSensorEnabled=true\n    mShowingLockscreen=%s mShowingDream=false mDreamingLockscreen=false\n    mKeyguard=Window{42ab1528 u0 Keyguard}\n    mDismissKeyguard=0 mWinDismissingKeyguard=null mHomePressed=false\n"

func TestCheckAwake(t *testing.T) {
	tests := []struct {
		name, power, policy, wantErr string
	}{
		{"asleep", powerAsleep, policyUnlocked, "Asleep"},
		{"locked", powerAwake, policyLocked, "keyguard is showing"},
		{"legacy unlocked", powerAwake, fmt.Sprintf(policyLegacy, "false"), ""},
		{"legacy locked", powerAwake, fmt.Sprintf(policyLegacy, "true"), "keyguard is showing"},
		{"unknown format passes", powerAwake, "WINDOW MANAGER POLICY STATE\n    mSystemBooted=true\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			fakeShell(s, map[string]string{"dumpsys power": tt.power, "dumpsys window policy": tt.policy})
			err := s.checkAwake(context.Background(), "A")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func countOf(cmds []string, want string) int {
	n := 0
	for _, c := range cmds {
		if c == want {
			n++
		}
	}
	return n
}
