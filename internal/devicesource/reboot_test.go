package devicesource

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
)

func TestRebootIfStaleBelowLimitDoesNothing(t *testing.T) {
	s := &Server{rebootAfter: 24 * time.Hour}
	cmds := fakeShell(s, map[string]string{"cat /proc/uptime": "3600.50 7000.00\n"})
	resp, err := s.RebootIfStale(context.Background(), &pb.RebootRequest{Serial: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rebooted || resp.UptimeSeconds != 3600 {
		t.Errorf("resp = %v, want not rebooted, uptime 3600", resp)
	}
	if want := []string{"cat /proc/uptime"}; !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands = %q, want %q", *cmds, want)
	}

	s.rebootAfter = 0 // off
	fakeShell(s, map[string]string{"cat /proc/uptime": "999999 0"})
	if resp, _ := s.RebootIfStale(context.Background(), &pb.RebootRequest{Serial: "A"}); resp.Rebooted {
		t.Error("rebooted with the limit off")
	}
}

// Over the limit: reboot, then wait until the phone answers with a new
// boot_id and sys.boot_completed=1, which is what proves it went down and
// came back rather than ignoring the command.
func TestRebootIfStaleRebootsAndWaitsForBoot(t *testing.T) {
	s := &Server{rebootAfter: 24 * time.Hour, rebootTimeout: time.Second}
	var cmds []string
	polls := 0
	s.shell = func(_ context.Context, _, command string) (string, error) {
		cmds = append(cmds, command)
		switch command {
		case "cat /proc/uptime":
			return "90000.00 1.00", nil
		case "dumpsys window policy":
			return policyUnlocked, nil
		case "cat /proc/sys/kernel/random/boot_id":
			return "old-boot\n", nil
		case "reboot":
			return "", errors.New("connection reset") // the phone went down mid-command
		case "cat /proc/sys/kernel/random/boot_id; getprop sys.boot_completed":
			polls++
			if polls == 1 {
				return "old-boot\n1\n", nil // not down yet: the old boot still answers
			}
			return "new-boot\n1\n", nil
		}
		return "", nil
	}
	resp, err := s.RebootIfStale(context.Background(), &pb.RebootRequest{Serial: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Rebooted || resp.UptimeSeconds != 90000 {
		t.Errorf("resp = %v, want rebooted, uptime 90000", resp)
	}
	if polls != 2 {
		t.Errorf("polled %d times, want 2", polls)
	}
	if countOf(cmds, "reboot") != 1 {
		t.Errorf("commands = %q, want one reboot", cmds)
	}
	if s.lastReboot("A").IsZero() {
		t.Error("last reboot not recorded")
	}
}

// No baseline boot_id, no reboot: without it the wait cannot tell the old
// boot from the new one.
func TestRebootIfStaleNeedsTheOldBootID(t *testing.T) {
	s := &Server{rebootAfter: time.Hour, rebootTimeout: time.Second}
	cmds := fakeShell(s, map[string]string{"cat /proc/uptime": "90000 1", "dumpsys window policy": policyUnlocked}, "cat /proc/sys/kernel/random/boot_id")
	if _, err := s.RebootIfStale(context.Background(), &pb.RebootRequest{Serial: "A"}); err == nil {
		t.Fatal("no error without a boot_id")
	}
	if countOf(*cmds, "reboot") != 0 {
		t.Errorf("rebooted without a boot_id: %q", *cmds)
	}
}

// A PIN phone boots to "unlock to start", which sair cannot get past, so it
// is never rebooted by uptime; nor is one whose dump cannot say.
func TestRebootIfStaleLeavesASecureLockedPhoneUp(t *testing.T) {
	for name, policy := range map[string]string{"secure": policySecure, "unknown format": fmt.Sprintf(policyLegacy, "false")} {
		s := &Server{rebootAfter: time.Hour, rebootTimeout: time.Second}
		cmds := fakeShell(s, map[string]string{"cat /proc/uptime": "90000 1", "dumpsys window policy": policy})
		resp, err := s.RebootIfStale(context.Background(), &pb.RebootRequest{Serial: "A"})
		if err != nil || resp.Rebooted {
			t.Fatalf("%s: resp = %v, err = %v; want not rebooted, no error", name, resp, err)
		}
		if countOf(*cmds, "reboot") != 0 {
			t.Errorf("%s: rebooted: %q", name, *cmds)
		}
	}
}

// Android 17 (Pixel 7, sair#135) has no secure= in the window policy, only
// simSecure=; dumpsys lock_settings still says whether a credential is set.
const (
	policyAndroid17 = "WINDOW MANAGER POLICY STATE (dumpsys window policy)\n    mKeyguardOccluded=false mPendingKeyguardOccluded=false\n    KeyguardServiceDelegate\n      showing=false\n      inputRestricted=false\n      occluded=false\n      trusted=false\n      simSecure=false\n      dreaming=false\n      systemReady=true\n      deviceHasKeyguard=true\n      enabled=true\n      userId=0\n      bootCompleted=true\n"
	lockSettings    = "Current lock settings service state:\n\nUser State:\n  User 0\n    LSKF-based SP protector ID: 0c48689f69bfe652\n    SID: 0000000000000000\n    Quality: 0\n    CredentialType: %s\n    IsLockScreenDisabled: false\n    IsUseOneLockSettingEnabled: true\n    Metrics: known\n"
)

func TestSecureLockFallsBackToLockSettings(t *testing.T) {
	for _, tt := range []struct{ policy, locks, want string }{
		{policyUnlocked, "", "false"},
		{policySecure, "", "true"},
		{policyAndroid17, fmt.Sprintf(lockSettings, "NONE"), "false"},
		{policyAndroid17, fmt.Sprintf(lockSettings, "PIN"), "true"}, // set and read on a moto g15
		{policyAndroid17, fmt.Sprintf(lockSettings, "PATTERN"), "true"},
		{policyAndroid17, "Can't find service: lock_settings\n", ""},
	} {
		s := &Server{}
		fakeShell(s, map[string]string{"dumpsys window policy": tt.policy, "dumpsys lock_settings": tt.locks})
		if got, err := s.secureLock(context.Background(), "A"); err != nil || got != tt.want {
			t.Errorf("secureLock(%q, %q) = %q, %v; want %q", tt.policy, tt.locks, got, err, tt.want)
		}
	}
}

// A radio whose reset failed is rebooted only when its service hangs too
// (sair#139): a dump that answers means the phone just ignores svc.
func TestRebootIfWedgedOnlyWhenTheServiceHangs(t *testing.T) {
	for _, hung := range []bool{false, true} {
		s := &Server{radioTimeout: 10 * time.Millisecond, rebootTimeout: time.Second}
		var cmds []string
		s.shell = func(ctx context.Context, _, command string) (string, error) {
			cmds = append(cmds, command)
			switch command {
			case "dumpsys wifi":
				if hung {
					<-ctx.Done()
					return "", ctx.Err()
				}
				return "Wi-Fi is enabled\n", nil
			case "dumpsys window policy":
				return policyUnlocked, nil
			case "cat /proc/sys/kernel/random/boot_id":
				return "old-boot\n", nil
			case "cat /proc/sys/kernel/random/boot_id; getprop sys.boot_completed":
				return "new-boot\n1\n", nil
			}
			return "", nil
		}
		resp, err := s.RebootIfWedged(context.Background(), &pb.WedgedRadioRequest{Serial: "A", Radio: "wifi"})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Rebooted != hung || (countOf(cmds, "reboot") == 1) != hung {
			t.Errorf("hung=%v: rebooted=%v, commands %q", hung, resp.Rebooted, cmds)
		}
	}
}
