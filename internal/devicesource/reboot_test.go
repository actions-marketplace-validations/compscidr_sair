package devicesource

import (
	"context"
	"errors"
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
	cmds := fakeShell(s, map[string]string{"cat /proc/uptime": "90000 1"}, "cat /proc/sys/kernel/random/boot_id")
	if _, err := s.RebootIfStale(context.Background(), &pb.RebootRequest{Serial: "A"}); err == nil {
		t.Fatal("no error without a boot_id")
	}
	if countOf(*cmds, "reboot") != 0 {
		t.Errorf("rebooted without a boot_id: %q", *cmds)
	}
}
