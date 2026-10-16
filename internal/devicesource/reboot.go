package devicesource

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RebootIfStale (sair#93) reboots a phone that has been up longer than
// REBOOT_UPTIME_HOURS and returns once it has booted. The proxy calls it on
// release, under the phone's reset lock, so the reboot happens while the
// phone is unacquired and the next acquire waits for it instead of getting
// a phone mid-boot. Waking and unlocking afterwards is the proxy's Wake call.
func (s *Server) RebootIfStale(ctx context.Context, req *pb.RebootRequest) (*pb.RebootResponse, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	out, err := s.shell(ctx, req.Serial, "cat /proc/uptime")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: uptime: %v", req.Serial, err)
	}
	uptime, err := parseUptime(out)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: uptime: %v", req.Serial, err)
	}
	resp := &pb.RebootResponse{UptimeSeconds: int64(uptime.Seconds())}
	if s.rebootAfter <= 0 || uptime < s.rebootAfter {
		return resp, nil
	}
	// A phone with a PIN, pattern or password boots to "unlock to start",
	// which no dismiss-keyguard gets past: a reboot would turn a phone that
	// works today into one a human has to visit. Leave it up and say so.
	secure, err := s.secureLock(ctx, req.Serial)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: %v", req.Serial, err)
	}
	switch secure {
	case "false":
	case "true":
		slog.Warn("uptime reboot skipped: a secure lock is set, and sair could not unlock the phone after a reboot. Set Settings > Lock screen > Screen lock type to Swipe or None", "serial", req.Serial, "uptime", uptime.Round(time.Minute))
		return resp, nil
	default:
		slog.Warn("uptime reboot skipped: cannot tell from dumpsys window policy or lock_settings whether a secure lock is set on this Android version", "serial", req.Serial, "uptime", uptime.Round(time.Minute))
		return resp, nil
	}
	// The old boot_id is the whole proof that the phone went down; without
	// it the first poll would pass on the old boot, so no reboot without it.
	before, err := s.shell(ctx, req.Serial, "cat /proc/sys/kernel/random/boot_id")
	if before = strings.TrimSpace(before); err != nil || before == "" {
		return nil, status.Errorf(codes.Internal, "%s: boot_id before reboot: %v; not rebooting", req.Serial, err)
	}
	slog.Info("rebooting: uptime over the limit", "serial", req.Serial, "uptime", uptime.Round(time.Minute), "limit", s.rebootAfter)
	// The connection drops as the phone goes down, so the command's own
	// error says nothing; the boot_id changing is what proves it rebooted.
	s.shell(ctx, req.Serial, "reboot")
	err = s.waitFor(ctx, s.rebootTimeout, req.Serial+" not back after reboot", func() error {
		out, err := s.shell(ctx, req.Serial, "cat /proc/sys/kernel/random/boot_id; getprop sys.boot_completed")
		if err != nil {
			return err
		}
		id, completed, _ := strings.Cut(strings.TrimSpace(out), "\n")
		switch {
		case strings.TrimSpace(id) == before:
			return fmt.Errorf("still the old boot")
		case strings.TrimSpace(completed) != "1":
			return fmt.Errorf("sys.boot_completed=%q", strings.TrimSpace(completed))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	resp.Rebooted = true
	s.stateMu.Lock()
	if s.reboots == nil {
		s.reboots = make(map[string]time.Time)
	}
	s.reboots[req.Serial] = time.Now()
	s.stateMu.Unlock()
	slog.Info("rebooted", "serial", req.Serial)
	return resp, nil
}

// secureLock is "true" when serial has a PIN, pattern or password, "false"
// when its lock screen is Swipe or None, and "" when no dump says.
func (s *Server) secureLock(ctx context.Context, serial string) (string, error) {
	out, err := s.shell(ctx, serial, "dumpsys window policy")
	if err != nil {
		return "", fmt.Errorf("dumpsys window policy: %w", err)
	}
	if v := dumpsysField(out, "secure="); v != "" {
		return v, nil
	}
	// Android 17 dropped secure= from the window policy (sair#135).
	// lock_settings still has the credential type, which the shell can
	// read: NONE for Swipe or None, else PIN, PATTERN, PASSWORD, ...
	out, err = s.shell(ctx, serial, "dumpsys lock_settings")
	if err != nil {
		return "", fmt.Errorf("dumpsys lock_settings: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "CredentialType:"); ok {
			return strconv.FormatBool(strings.TrimSpace(v) != "NONE"), nil
		}
	}
	return "", nil
}

// parseUptime reads /proc/uptime: "<seconds up> <idle seconds>".
func parseUptime(out string) (time.Duration, error) {
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("empty /proc/uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}
