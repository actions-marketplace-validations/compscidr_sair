package devicesource

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
)

// What the probe prints on a leaked Pixel 7a (Android 16), captured on the
// bench: "Killed" from the shell, then the crash line from the probe's logcat.
const uiaLeak = "Killed \n10-16 14:37:45.562 28326 28326 E AndroidRuntime: java.lang.IllegalStateException: UiAutomationService android.accessibilityservice.IAccessibilityServiceClient$Stub$Proxy@9161eedalready registered!\n"

func TestClearUiAutomationNoLeakDoesNothing(t *testing.T) {
	s := &Server{}
	cmds := fakeShell(s, map[string]string{uiaProbe: "UI hierchary dumped to: /data/local/tmp/sair-uia-probe.xml\n"})
	resp, err := s.ClearUiAutomation(context.Background(), &pb.UiAutomationRequest{Serial: "A"})
	if err != nil || resp.Cleared != "" {
		t.Fatalf("resp = %v, err = %v; want nothing cleared", resp, err)
	}
	if want := []string{uiaProbe}; !reflect.DeepEqual(*cmds, want) {
		t.Errorf("commands = %q, want %q", *cmds, want)
	}
}

// A leak goes by killing the shell's app_process holders; the reboot is not
// needed. Toolbox ps (Android < 8) prints nothing for -A, so plain ps is read.
func TestClearUiAutomationKillsHolders(t *testing.T) {
	s := &Server{}
	probes := 0
	var cmds []string
	s.shell = func(_ context.Context, _, command string) (string, error) {
		cmds = append(cmds, command)
		switch command {
		case uiaProbe:
			if probes++; probes == 1 {
				return uiaLeak, nil
			}
			return "UI hierchary dumped to: /data/local/tmp/sair-uia-probe.xml\n", nil
		case "ps -A":
			return "USER     PID   PPID  VSIZE  RSS     WCHAN    PC         NAME\n", nil
		case "ps":
			return "USER     PID   PPID  VSIZE  RSS     WCHAN    PC         NAME\n" +
				"root      1     0     8436   592   ffffffff 00000000 S /init\n" +
				"root      180   1     902432 48000 ffffffff 00000000 S zygote\n" +
				"u0_a52    2210  180   951240 61000 ffffffff 00000000 S com.example.app\n" +
				"shell     2190  2180  828692 37000 ffffffff 00000000 S app_process\n" +
				"shell     2300  1     828692 37000 ffffffff 00000000 S uiautomator\n" +
				"shell     2400  2399  2560   900   ffffffff 00000000 S logcat\n", nil
		}
		return "", nil
	}
	resp, err := s.ClearUiAutomation(context.Background(), &pb.UiAutomationRequest{Serial: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "killed 2190 app_process, 2300 uiautomator"; resp.Cleared != want {
		t.Errorf("cleared = %q, want %q", resp.Cleared, want)
	}
	var kills []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "kill") || c == "reboot" {
			kills = append(kills, c)
		}
	}
	if want := []string{"kill -9 2190", "kill -9 2300"}; !reflect.DeepEqual(kills, want) {
		t.Errorf("kills = %q, want %q", kills, want)
	}
}

// Killing nothing helps (the holder is not a shell process): reboot, verified.
func TestClearUiAutomationRebootsAsLastResort(t *testing.T) {
	s := &Server{rebootTimeout: time.Second}
	rebooted := false
	s.shell = func(_ context.Context, _, command string) (string, error) {
		switch command {
		case uiaProbe:
			if rebooted {
				return "", nil
			}
			return uiaLeak, nil
		case "ps -A":
			return "USER PID PPID VSZ RSS WCHAN ADDR S NAME\nroot 1 0 1 1 0 0 S init\n", nil
		case "dumpsys window policy":
			return policyUnlocked, nil
		case "cat /proc/sys/kernel/random/boot_id":
			return "old-boot\n", nil
		case "reboot":
			rebooted = true
		case "cat /proc/sys/kernel/random/boot_id; getprop sys.boot_completed":
			return "new-boot\n1\n", nil
		}
		return "", nil
	}
	resp, err := s.ClearUiAutomation(context.Background(), &pb.UiAutomationRequest{Serial: "A"})
	if err != nil || resp.Cleared != "rebooted" {
		t.Fatalf("resp = %v, err = %v; want rebooted", resp, err)
	}
	if s.lastReboot("A").IsZero() {
		t.Error("reboot not recorded")
	}
}

func TestUiaHoldersToybox(t *testing.T) {
	ps := "USER           PID  PPID     VSZ    RSS WCHAN            ADDR S NAME\n" +
		"root             1     0 1 1 0 0 S init\n" +
		"shell        12345   400 1 1 0 0 S app_process64\n" +
		"shell        12400   400 1 1 0 0 S sh\n" +
		"u0_a1         5000   300 1 1 0 0 S app_process\n"
	if got, want := uiaHolders(ps), []string{"12345 app_process64"}; !reflect.DeepEqual(got, want) {
		t.Errorf("holders = %q, want %q", got, want)
	}
}
