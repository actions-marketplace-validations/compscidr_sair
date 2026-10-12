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

// Real One UI output, Android 14 and 16 (cube bench, Oct 2026).
const (
	powerAwake     = "POWER MANAGER (dumpsys power)\n  mWakefulness=Awake\n  mWakefulnessChanging=false\n  mStayOn=true\n"
	powerAsleep    = "POWER MANAGER (dumpsys power)\n  mWakefulness=Asleep\n  mWakefulnessChanging=false\n"
	policyUnlocked = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    mKeyguardOccluded=false mKeyguardOccludedChanged=false\n    KeyguardServiceDelegate\n      showing=false\n      inputRestricted=false\n      occluded=false\n      KeyguardStateMonitor\n        mIsShowing=false\n"
	policyLocked   = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    KeyguardServiceDelegate\n      showing=true\n      KeyguardStateMonitor\n        mIsShowing=true\n"
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

func TestCheckAwakeNamesWhatIsWrong(t *testing.T) {
	s := &Server{}
	fakeShell(s, map[string]string{"dumpsys power": powerAsleep, "dumpsys window policy": policyUnlocked})
	if err := s.checkAwake(context.Background(), "A"); err == nil || !strings.Contains(err.Error(), "Asleep") {
		t.Errorf("asleep: err = %v", err)
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
