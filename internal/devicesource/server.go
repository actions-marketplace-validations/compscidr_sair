package devicesource

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compscidr/sair/internal/logging"
	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

var serialPattern = regexp.MustCompile(`^[a-zA-Z0-9._:()\-]+$`)

// Server implements the DeviceSource gRPC service.
type Server struct {
	pb.UnimplementedDeviceSourceServer
	adbPort int

	// Bench Wi-Fi (see SetWifi). An empty SSID means Wi-Fi is not managed.
	wifiSSID, wifiPassphrase, wifiPingHost string
	// wifiTimeout bounds ON (join and validate); wifiOffTimeout bounds OFF
	// and RESET (disassociate, radio back).
	wifiTimeout, wifiOffTimeout time.Duration
	// shell runs a command line on a device; adbShell, swapped in tests.
	shell func(ctx context.Context, serial, command string) (string, error)

	// wakeTimeout bounds one wake + dismiss-keyguard attempt (see wake).
	wakeTimeout time.Duration
	// rebootAfter is REBOOT_UPTIME_HOURS; 0 disables the uptime reboot.
	// rebootTimeout bounds the wait for a phone to be back.
	rebootAfter, rebootTimeout time.Duration
	// Per-serial screen outcome (Device.warning) and last sair reboot.
	stateMu  sync.Mutex
	warnings map[string]string
	reboots  map[string]time.Time

	// Forward watchdog (see forwardWatch); 0 means the default.
	stallTimeout, idleWarnInterval time.Duration

	// Build props per attached serial (see buildProps).
	propsMu sync.Mutex
	props   map[string]buildProps

	// Account types per serial, refreshed every accountRefresh (see accountTypes).
	accountsMu sync.Mutex
	accounts   map[string]accountsEntry

	// Baseline package set per serial (see packages.go); baselineOverride,
	// from BASELINE_PACKAGES, replaces it for every device when set.
	baselineMu       sync.Mutex
	baseline         map[string]map[string]struct{}
	baselineWarned   map[string]bool // serials whose baseline read already failed loudly
	baselineOverride map[string]struct{}
	// Apps force-stopped (RESET_FORCE_STOP) and cleared (RESET_CLEAR_PACKAGES)
	// on release, so browser and mail state does not carry over.
	stopPackages, clearPackages []string

	// Serial filter (sair#90): with allow set only those serials are managed;
	// otherwise everything but deny. A filtered phone is never reported,
	// locked or reset, and every per-device RPC refuses it. filterLogged is
	// the last filter outcome logged, so the log line only repeats on change.
	allow, deny  map[string]struct{}
	filterMu     sync.Mutex // GetDevices runs from the reporter and from any gRPC caller at once
	filterLogged string
}

func NewServer() *Server {
	port := 5038
	if v := os.Getenv("ADB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	s := &Server{
		adbPort:        port,
		wifiSSID:       os.Getenv("WIFI_SSID"),
		wifiPassphrase: os.Getenv("WIFI_PASSPHRASE"),
		wifiPingHost:   "dns.google", // a hostname, so the check proves DNS too
		wifiTimeout:    60 * time.Second,
		wifiOffTimeout: 20 * time.Second,
		wakeTimeout:    5 * time.Second,
		rebootAfter:    24 * time.Hour,
		rebootTimeout:  5 * time.Minute,
		stopPackages:   packageList("com.android.chrome,com.google.android.gm"),
	}
	if v, err := strconv.ParseFloat(os.Getenv("REBOOT_UPTIME_HOURS"), 64); err == nil && v >= 0 {
		s.rebootAfter = time.Duration(v * float64(time.Hour))
	}
	if v, ok := os.LookupEnv("RESET_FORCE_STOP"); ok {
		s.stopPackages = packageList(v)
	}
	s.clearPackages = packageList(os.Getenv("RESET_CLEAR_PACKAGES"))
	if v, ok := os.LookupEnv("BASELINE_PACKAGES"); ok {
		s.baselineOverride = packageSet(packageList(v))
		if s.baselineOverride == nil {
			s.baselineOverride = map[string]struct{}{} // set but empty: keep nothing third-party
		}
	}
	if v := os.Getenv("WIFI_PING_HOST"); v != "" {
		s.wifiPingHost = v
	}
	if v, err := strconv.Atoi(os.Getenv("WIFI_CONNECT_TIMEOUT_SECONDS")); err == nil && v > 0 {
		s.wifiTimeout = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("WIFI_DISCONNECT_TIMEOUT_SECONDS")); err == nil && v > 0 {
		s.wifiOffTimeout = time.Duration(v) * time.Second
	}
	if v, ok := os.LookupEnv("DEVICE_SERIALS"); ok {
		s.allow = packageSet(packageList(v))
		if s.allow == nil {
			s.allow = map[string]struct{}{} // set but empty: manage nothing
		}
	}
	s.deny = packageSet(packageList(os.Getenv("EXCLUDE_SERIALS")))
	s.shell = s.adbShell
	return s
}

// managed reports whether the serial filter lets this device source touch a
// phone.
func (s *Server) managed(serial string) bool {
	if s.allow != nil {
		_, ok := s.allow[serial]
		return ok
	}
	_, denied := s.deny[serial]
	return !denied
}

// checkSerial is validateSerial plus the serial filter, for every per-device
// RPC: a filtered phone is as good as unplugged.
func (s *Server) checkSerial(serial string) error {
	if err := validateSerial(serial); err != nil {
		return err
	}
	if !s.managed(serial) {
		return status.Errorf(codes.NotFound, "device %s is excluded by DEVICE_SERIALS/EXCLUDE_SERIALS", serial)
	}
	return nil
}

func (s *Server) adbCmd(args ...string) *exec.Cmd {
	fullArgs := append([]string{"-P", strconv.Itoa(s.adbPort)}, args...)
	return exec.Command("adb", fullArgs...)
}

func (s *Server) getSerialNumbers() ([]string, error) {
	cmd := s.adbCmd("devices")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	return s.filterSerials(parseAdbDevices(string(out))), nil
}

// parseAdbDevices reads `adb devices`: the serials in state "device".
func parseAdbDevices(out string) []string {
	var serials []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == "device" {
			serials = append(serials, parts[0])
		}
	}
	return serials
}

// filterSerials applies DEVICE_SERIALS / EXCLUDE_SERIALS and logs, whenever
// the outcome changes, which attached phones are left out and which
// allowlisted ones are not attached (a typo in the list shows up here, not as
// a bench with fewer phones).
func (s *Server) filterSerials(attached []string) []string {
	if s.allow == nil && s.deny == nil {
		return attached
	}
	var kept, filtered, missing []string
	for _, serial := range attached {
		if s.managed(serial) {
			kept = append(kept, serial)
		} else {
			filtered = append(filtered, serial)
		}
	}
	for serial := range s.allow {
		if !slices.Contains(attached, serial) {
			missing = append(missing, serial)
		}
	}
	slices.Sort(missing)
	key := fmt.Sprint(filtered, missing)
	s.filterMu.Lock()
	changed := key != s.filterLogged
	s.filterLogged = key
	s.filterMu.Unlock()
	if changed {
		slog.Info("serial filter", "managed", kept, "filteredOut", filtered)
		if len(missing) > 0 {
			slog.Warn("DEVICE_SERIALS names phones that are not attached", "serials", missing)
		}
	}
	return kept
}

func validateSerial(serial string) error {
	if serial == "" || !serialPattern.MatchString(serial) {
		return status.Errorf(codes.InvalidArgument, "invalid device serial: %s", serial)
	}
	return nil
}

func (s *Server) getDeviceInfo(serial string) (*pb.Device, error) {
	if err := validateSerial(serial); err != nil {
		return nil, err
	}

	p := s.buildProps(serial)
	release, _ := strconv.Atoi(p.release)
	sdk, _ := strconv.Atoi(p.sdk)
	s.recordBaseline(serial)

	var lastReboot int64
	if t := s.lastReboot(serial); !t.IsZero() {
		lastReboot = t.UnixMilli()
	}
	return &pb.Device{
		Serial:       serial,
		Manufacturer: p.manufacturer,
		Model:        p.model,
		Release:      int32(release),
		Sdk:          int32(sdk),
		AccountTypes: s.accountTypes(serial),
		LastRebootMs: lastReboot,
		Warning:      s.warning(serial),
	}, nil
}

type buildProps struct{ model, manufacturer, release, sdk string }

// buildProps reads a phone's build props in one shell, once per attachment:
// they cannot change while it stays attached, and a device report that runs
// adb shells every 5 s falls behind on a busy host until the proxy drops the
// source (sair#104). A failed read is not kept, so the next report retries.
func (s *Server) buildProps(serial string) buildProps {
	s.propsMu.Lock()
	p, ok := s.props[serial]
	s.propsMu.Unlock()
	if ok {
		return p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := s.shell(ctx, serial, "getprop ro.product.model; getprop ro.product.manufacturer; getprop ro.build.version.release; getprop ro.build.version.sdk")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(lines) != 4 {
		slog.Debug("reading build props failed", "serial", serial, "error", err, "output", out)
		return buildProps{}
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	p = buildProps{lines[0], lines[1], lines[2], lines[3]}
	s.propsMu.Lock()
	if s.props == nil {
		s.props = make(map[string]buildProps)
	}
	s.props[serial] = p
	s.propsMu.Unlock()
	return p
}

// forgetProps drops the build props of phones no longer attached, so one
// that comes back (reflashed, say) is read again.
func (s *Server) forgetProps(attached []string) {
	s.propsMu.Lock()
	defer s.propsMu.Unlock()
	for serial := range s.props {
		if !slices.Contains(attached, serial) {
			delete(s.props, serial)
		}
	}
}

func (s *Server) GetDevices(_ context.Context, _ *emptypb.Empty) (*pb.Devices, error) {
	serials, err := s.getSerialNumbers()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list devices: %v", err)
	}
	s.forgetProps(serials)

	devices := &pb.Devices{}
	for _, serial := range serials {
		device, err := s.getDeviceInfo(serial)
		if err != nil {
			slog.Warn("failed to get device info", "serial", serial, "error", err)
			continue
		}
		devices.Devices = append(devices.Devices, device)
	}
	return devices, nil
}

// EnqueueCommand executes an arbitrary command via bash.
// This is intentional — the caller is authenticated via the orchestrator's
// API key, and the command is expected to be a full shell invocation
// (e.g. "./gradlew connectedCheck"). Do not expose this endpoint without auth.
func (s *Server) EnqueueCommand(req *pb.Command, stream pb.DeviceSource_EnqueueCommandServer) error {
	return s.streamProcess(stream.Context(), exec.Command("bash", "-c", req.Cmd), 1800, stream)
}

func (s *Server) ExecOnDevice(req *pb.DeviceCommand, stream pb.DeviceSource_ExecOnDeviceServer) error {
	if err := s.checkSerial(req.Serial); err != nil {
		return err
	}

	args := []string{"-P", strconv.Itoa(s.adbPort), "-s", req.Serial, "shell"}
	args = append(args, strings.Fields(req.Command)...)
	slog.Info("ExecOnDevice", "args", logging.RedactSecrets(strings.Join(args, " ")))

	timeout := int(req.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 1800
	}

	cmd := exec.Command("adb", args...)
	return s.streamProcess(stream.Context(), cmd, timeout, stream)
}

type commandResultStream interface {
	Send(*pb.CommandResult) error
}

func (s *Server) streamProcess(ctx context.Context, cmd *exec.Cmd, timeoutSeconds int, stream commandResultStream) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start: %v", err)
	}

	// Stream stdout and stderr concurrently
	readersDone := make(chan error, 2)

	streamReader := func(reader io.Reader, isStderr bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text() + "\n"
			result := &pb.CommandResult{}
			if isStderr {
				result.Stderr = line
			} else {
				result.Stdout = line
			}
			if err := stream.Send(result); err != nil {
				readersDone <- err
				return
			}
		}
		readersDone <- scanner.Err()
	}

	go streamReader(stdout, false)
	go streamReader(stderr, true)

	// Monitor process completion, timeout, and ctx cancellation concurrently.
	// Killing the process closes the pipes, which unblocks the readers.
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	timer := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
		cmd.Process.Kill()
		slog.Warn("process timed out", "timeout", timeoutSeconds)
		// Wait for readers to drain after kill
		for i := 0; i < 2; i++ {
			<-readersDone
		}
		<-waitDone
		stream.Send(&pb.CommandResult{
			Stderr:   fmt.Sprintf("Command timed out after %ds", timeoutSeconds),
			ExitCode: -1,
		})
	case <-ctx.Done():
		cmd.Process.Kill()
		for i := 0; i < 2; i++ {
			<-readersDone
		}
		<-waitDone
	case err := <-waitDone:
		// Process exited — wait for readers to finish draining output
		for i := 0; i < 2; i++ {
			<-readersDone
		}
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		stream.Send(&pb.CommandResult{ExitCode: int32(exitCode)})
	}

	return nil
}

func (s *Server) ForwardToDevice(stream pb.DeviceSource_ForwardToDeviceServer) error {
	// First message must be setup
	firstMsg, err := stream.Recv()
	if err != nil {
		return err
	}
	setup := firstMsg.GetSetup()
	if setup == nil {
		return status.Error(codes.InvalidArgument, "first message must be setup")
	}
	if err := s.checkSerial(setup.Serial); err != nil {
		return err
	}

	slog.Info("ForwardToDevice: connecting to ADB server", "serial", setup.Serial, "command", logging.RedactSecrets(setup.InitialCommand))

	// Connect to real ADB server
	conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", s.adbPort))
	if err != nil {
		return status.Errorf(codes.Internal, "failed to connect to ADB server: %v", err)
	}

	// Establish transport to the device
	if err := sendAdbLtv(conn, "host:transport:"+setup.Serial); err != nil {
		conn.Close()
		return status.Errorf(codes.Internal, "failed to send transport: %v", err)
	}
	if err := readAdbOkay(conn); err != nil {
		conn.Close()
		return err
	}
	slog.Debug("ForwardToDevice: transport established", "serial", setup.Serial)

	// Send the initial command if provided (e.g., "sync:").
	// When empty, the proxy is tunneling everything after transport — the
	// client's next message will be the command, relayed through the tunnel.
	if setup.InitialCommand != "" {
		if err := sendAdbLtv(conn, setup.InitialCommand); err != nil {
			conn.Close()
			return status.Errorf(codes.Internal, "failed to send initial command: %v", err)
		}
		slog.Debug("ForwardToDevice: initial command sent", "serial", setup.Serial, "command", logging.RedactSecrets(setup.InitialCommand))
	}

	// Bidirectional forwarding, watched for stalls (sair#79).
	done := make(chan error, 1)
	w := newForwardWatch(setup.Serial, setup.InitialCommand)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go w.run(conn, s.stallTimeout, s.idleWarnInterval, stopWatch)

	// ADB → gRPC (response direction — drives teardown)
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				slog.Debug("ForwardToDevice: ADB read done", "serial", setup.Serial, "error", err)
				done <- err
				return
			}
			w.touch()
			slog.Debug("ForwardToDevice: ADB→gRPC", "serial", setup.Serial, "bytes", n)
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&pb.ForwardData{
				Payload: &pb.ForwardData_Data{Data: data},
			}); err != nil {
				slog.Debug("ForwardToDevice: gRPC send failed", "serial", setup.Serial, "error", err)
				done <- err
				return
			}
		}
	}()

	// gRPC → ADB (request direction — half-close aware)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				slog.Info("ForwardToDevice: gRPC recv done", "serial", setup.Serial, "error", err)
				if err == io.EOF {
					// Client done sending — half-close the ADB write side so ADB
					// knows all data has arrived, but keep the read side open for
					// the response (e.g. exec: install-write result).
					if tc, ok := conn.(*net.TCPConn); ok {
						slog.Info("ForwardToDevice: half-closing ADB write side", "serial", setup.Serial)
						tc.CloseWrite()
					}
				} else {
					// Real error — close conn to unblock the response direction.
					conn.Close()
				}
				return
			}
			if data := msg.GetData(); data != nil {
				slog.Debug("ForwardToDevice: gRPC→ADB", "serial", setup.Serial, "bytes", len(data))
				w.writing()
				_, err := conn.Write(data)
				w.touch()
				if err != nil {
					slog.Info("ForwardToDevice: ADB write failed", "serial", setup.Serial, "error", err)
					conn.Close()
					return
				}
			}
		}
	}()

	// Wait for the response direction to finish, then close the ADB connection
	// and return. Returning from the method causes the gRPC framework to cancel
	// the stream, which unblocks the request goroutine's stream.Recv().
	err = <-done
	conn.Close()

	if reason := w.stalled(); reason != "" {
		slog.Warn("ForwardToDevice: stalled forward torn down", "serial", setup.Serial, "command", logging.RedactSecrets(setup.InitialCommand), "reason", reason)
		return status.Error(codes.DeadlineExceeded, reason)
	}
	resultErr := firstRealError(err)
	slog.Info("ForwardToDevice: finished", "serial", setup.Serial, "error", resultErr)
	return resultErr
}

// firstRealError returns the first non-benign error, or nil if both are benign
// (nil, io.EOF, or gRPC Canceled).
func firstRealError(errs ...error) error {
	for _, err := range errs {
		if err == nil || err == io.EOF {
			continue
		}
		if st, ok := status.FromError(err); ok && st.Code() == codes.Canceled {
			continue
		}
		return err
	}
	return nil
}

func sendAdbLtv(conn net.Conn, command string) error {
	lengthHex := fmt.Sprintf("%04X", len(command))
	if _, err := conn.Write([]byte(lengthHex)); err != nil {
		return err
	}
	_, err := conn.Write([]byte(command))
	return err
}

func readAdbOkay(conn net.Conn) error {
	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return status.Errorf(codes.Internal, "ADB server closed connection")
	}
	if string(resp) != "OKAY" {
		// Read the error message
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, lenBuf); err == nil {
			if length, err := strconv.ParseInt(string(lenBuf), 16, 32); err == nil {
				msg := make([]byte, length)
				io.ReadFull(conn, msg)
				return status.Errorf(codes.Internal, "ADB transport failed: %s: %s", string(resp), string(msg))
			}
		}
		return status.Errorf(codes.Internal, "ADB transport failed: %s", string(resp))
	}
	return nil
}

// RecentLog returns this process's recent log lines about one device, for
// the proxy to attach to a lock log when an acquire or reset fails on it.
func (s *Server) RecentLog(_ context.Context, req *pb.RecentLogRequest) (*pb.RecentLogResponse, error) {
	if err := s.checkSerial(req.Serial); err != nil {
		return nil, err
	}
	return &pb.RecentLogResponse{Lines: logging.Recent.Lines(req.Serial)}, nil
}
