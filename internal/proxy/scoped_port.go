package proxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ScopedPort represents a scoped ADB listener port for a specific lock.
type ScopedPort struct {
	LockID        string
	Serials       map[string]struct{}
	Port          int
	listener      net.Listener
	CreatedAt     time.Time
	stopCh        chan struct{}
	heartbeatStop chan struct{}
	// keepaliveDone is closed when runKeepalive returns, so CloseScopedPort can
	// wait for it before a caller drains the log (nothing left flushing/requeuing).
	keepaliveDone chan struct{}
	// Log of ADB requests made on this port, shipped to the orchestrator on
	// each heartbeat and on release.
	Log *LockLog
	// RemoteDevices are granted devices reached through the relay, keyed by
	// serial. nil for none.
	RemoteDevices map[string]*pb.DeviceInfo
	// DeviceToken lets a locked phone toggle its own Wi-Fi through POST /wifi
	// (sair#83). Random per lock, dies with it, good for nothing else.
	DeviceToken string
	// DeviceAPIPort is the port on each local phone that reaches the proxy's
	// HTTP API (an adb reverse set up on acquire); 0 when none was set up.
	DeviceAPIPort int
	// Stable transport ids for RemoteDevices, disjoint from the tracker's ids
	// (which start at 1). One mutex guards both directions so they can never
	// disagree: an id is only ever visible together with its reverse entry.
	remoteMu       sync.Mutex
	remoteBySerial map[string]int
	remoteByID     map[int]string
}

// remoteTransportID returns a transport id for a remote serial, stable for the
// port's lifetime. 0 for a serial that is not one of this port's remote devices.
func (sp *ScopedPort) remoteTransportID(serial string) int {
	if _, ok := sp.RemoteDevices[serial]; !ok {
		return 0
	}
	sp.remoteMu.Lock()
	defer sp.remoteMu.Unlock()
	if id, ok := sp.remoteBySerial[serial]; ok {
		return id
	}
	if sp.remoteBySerial == nil {
		sp.remoteBySerial = map[string]int{}
		sp.remoteByID = map[int]string{}
	}
	id := 1_000_000 + len(sp.remoteBySerial) + 1
	sp.remoteBySerial[serial] = id
	sp.remoteByID[id] = serial
	return id
}

// remoteSerialByTransportID is the reverse of remoteTransportID, for
// host:transport-id: on a remote device's id. Returns "" if unknown.
func (sp *ScopedPort) remoteSerialByTransportID(id int) string {
	sp.remoteMu.Lock()
	defer sp.remoteMu.Unlock()
	return sp.remoteByID[id]
}

// ScopedPortManager manages scoped ADB listener ports for per-runner device isolation.
//
// Each lock gets a dedicated TCP port that only exposes the locked devices.
// The bare port (5037) shows no devices; runners must acquire a scoped port
// via the proxy HTTP API to access any device.
type ScopedPortManager struct {
	mu                    sync.Mutex
	commandRouter         *CommandRouter
	deviceListTracker     *DeviceListTracker
	heartbeatIntervalSecs int64
	// How often buffered log entries are shipped on the session. Tests shorten it.
	flushInterval time.Duration
	scopedPorts   map[string]*ScopedPort
	// setWifi is commandRouter.SetWifi; tests swap it.
	setWifi func(serial string, state dspb.WifiRequest_State) error
	// removeReverse is commandRouter.RemoveReverseForwards; tests swap it.
	removeReverse func(serial string) error
	// addReverse is commandRouter.AddReverseForward; tests swap it.
	addReverse func(serial string, port int) error
	// resetPackages is commandRouter.ResetPackages; tests swap it.
	resetPackages func(serial string) (removed, suggestions []string, err error)
	// DeviceAPIPort is the proxy's HTTP API port, reverse-forwarded onto each
	// local phone on acquire so a test can call POST /wifi with the lock's
	// device token. 0 disables it. The forward lands on the device source's
	// host, so the proxy must serve its HTTP API there (they share a bench).
	DeviceAPIPort int
	// wifiMu holds one *sync.Mutex per serial. It orders Wi-Fi changes on a
	// device so a released lock's reset can never land after the next lock's
	// connect, while one phone's ON wait never blocks another phone's OFF.
	// ponytail: entries are never dropped; one per serial ever seen is fine.
	wifiMu sync.Map
}

func (m *ScopedPortManager) wifiLock(serial string) *sync.Mutex {
	mu, _ := m.wifiMu.LoadOrStore(serial, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func NewScopedPortManager(
	commandRouter *CommandRouter,
	deviceListTracker *DeviceListTracker,
	heartbeatIntervalSecs int64,
) *ScopedPortManager {
	if heartbeatIntervalSecs <= 0 {
		heartbeatIntervalSecs = 60
	}
	return &ScopedPortManager{
		commandRouter:         commandRouter,
		deviceListTracker:     deviceListTracker,
		heartbeatIntervalSecs: heartbeatIntervalSecs,
		flushInterval:         time.Second,
		scopedPorts:           make(map[string]*ScopedPort),
		setWifi:               commandRouter.SetWifi,
		removeReverse:         commandRouter.RemoveReverseForwards,
		addReverse:            commandRouter.AddReverseForward,
		resetPackages:         commandRouter.ResetPackages,
	}
}

// Acquire acquires a lock from the orchestrator (via gRPC) and opens a scoped ADB port.
// Blocks until the orchestrator grants the lock.
//
// Pass requestedSerials for specific devices, or count for that many arbitrary
// free devices. Passing neither locks every device in the tenant's pool.
//
// With wifi, every local device is joined to the bench Wi-Fi and online on it
// before Acquire returns; without it, every local device is verified offline
// first rather than trusting the previous release's reset (sair#86). Either
// way a device that cannot be put in that state releases the lock and
// returns the error. A bench without managed Wi-Fi skips this.
//
// With sameProxy, the orchestrator grants every device from a single proxy.
func (m *ScopedPortManager) Acquire(requestedSerials map[string]struct{}, count int32, repo, runURL string, priority int32, wifi, sameProxy bool, tenantKey string) (*ScopedPort, error) {
	result, err := m.commandRouter.AcquireLock(requestedSerials, count, 30, repo, runURL, priority, sameProxy, tenantKey)
	if err != nil {
		return nil, err
	}
	sp, err := m.CreateScopedPort(result.LockID, result.Serials, result.RemoteDevices)
	if err != nil {
		// Clean up: release the lock if we can't create the port
		if _, releaseErr := m.commandRouter.ReleaseLock(result.LockID, "error", nil); releaseErr != nil {
			slog.Warn("failed to release lock after scoped port creation failure",
				"lockId", result.LockID, "error", releaseErr)
		}
		return nil, err
	}
	m.setupDeviceAPI(sp)
	if err := m.SetWifi(sp.LockID, nil, wifi); err != nil && !wifiUnmanaged(err) {
		m.Release(sp.LockID, "error")
		return nil, err
	}
	return sp, nil
}

// setupDeviceAPI reverse-forwards the proxy's HTTP API onto every local phone
// of the lock. Best effort: a phone that refuses the forward only costs the
// job the device-side control, not the lock. Nothing to do for a relayed
// device: its adb reverse would end on the other proxy's host.
func (m *ScopedPortManager) setupDeviceAPI(sp *ScopedPort) {
	if m.DeviceAPIPort <= 0 {
		return
	}
	for serial := range sp.Serials {
		if _, remote := sp.RemoteDevices[serial]; remote {
			continue
		}
		if err := m.addReverse(serial, m.DeviceAPIPort); err != nil {
			slog.Warn("device API reverse forward failed; device-side Wi-Fi control unavailable", "serial", serial, "lockId", sp.LockID, "error", err)
			return
		}
	}
	sp.DeviceAPIPort = m.DeviceAPIPort
}

// DeviceTokenValid reports whether token is the live device token of lockID.
func (m *ScopedPortManager) DeviceTokenValid(lockID, token string) bool {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	return sp != nil && token != "" && subtle.ConstantTimeCompare([]byte(sp.DeviceToken), []byte(token)) == 1
}

var (
	errUnknownLock = errors.New("unknown lock_id")
	errNotInLock   = errors.New("serial is not part of this lock")
)

// SetWifi turns Wi-Fi on or off for serials of a lock, or for all of its
// local devices when serials is empty. Devices relayed from another proxy are
// not supported: naming one is an error, and "all" skips them.
func (m *ScopedPortManager) SetWifi(lockID string, serials []string, on bool) error {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	if sp == nil {
		return errUnknownLock
	}
	if len(serials) == 0 {
		for s := range sp.Serials {
			if _, remote := sp.RemoteDevices[s]; !remote {
				serials = append(serials, s)
			}
		}
	}
	for _, s := range serials {
		if _, ok := sp.Serials[s]; !ok {
			return fmt.Errorf("%w: %s", errNotInLock, s)
		}
	}
	state := dspb.WifiRequest_OFF
	if on {
		state = dspb.WifiRequest_ON
	}
	// Fan out: a lock's three phones take one join time, not three, and two
	// phones asking for opposite states at once (sair#84) do not queue.
	errs := make([]error, len(serials))
	var wg sync.WaitGroup
	for i, s := range serials {
		wg.Go(func() {
			mu := m.wifiLock(s)
			mu.Lock()
			defer mu.Unlock()
			errs[i] = m.setWifi(s, state)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// resetDevices returns a closed port's devices to baseline: no reverse
// forwards, the job's packages gone, Wi-Fi reset. A device a newer lock
// already holds is skipped: its job owns the state now.
func (m *ScopedPortManager) resetDevices(sp *ScopedPort) {
	for serial := range sp.Serials {
		if _, remote := sp.RemoteDevices[serial]; remote {
			continue
		}
		m.resetDevice(sp, serial)
	}
}

func (m *ScopedPortManager) resetDevice(sp *ScopedPort, serial string) {
	lockID := sp.LockID
	mu := m.wifiLock(serial)
	mu.Lock()
	defer mu.Unlock()
	// Checked under the lock: an expired lock's reset can queue behind the
	// next lock's ON on this phone, and must not then undo it.
	if m.heldByAnyPort(serial) {
		return
	}
	if err := m.removeReverse(serial); err != nil && status.Code(err) != codes.NotFound {
		slog.Warn("removing reverse forwards failed", "serial", serial, "lockId", lockID, "error", err)
	}
	// Packages before Wi-Fi: a suggestion a removed app registered for the
	// bench network goes with the app, so the reset's offline wait is not
	// spent on a phone that keeps re-joining (sair#76).
	removed, suggestions, err := m.resetPackages(serial)
	switch c := status.Code(err); {
	case err == nil && len(removed) > 0:
		slog.Info("uninstalled what the job left behind", "serial", serial, "lockId", lockID, "packages", removed, "suggestionsRemoved", suggestions)
		sp.Log.Record(&pb.LockLogEntry{TimestampMs: time.Now().UnixMilli(), Serial: serial, Service: packagesRemovedService(removed, suggestions)})
	case err == nil, c == codes.NotFound, c == codes.Unimplemented:
		// Nothing left behind, a relayed device, or an older device source.
	default:
		slog.Warn("package reset failed; the job's APKs stay on the phone", "serial", serial, "lockId", lockID, "error", err)
	}
	if err := m.setWifi(serial, dspb.WifiRequest_RESET); err != nil && !wifiUnmanaged(err) && status.Code(err) != codes.NotFound {
		slog.Warn("Wi-Fi reset failed", "serial", serial, "lockId", lockID, "error", err)
	}
}

// packagesRemovedService is the lock log line for a release's cleanup, e.g.
// "sair: uninstalled com.example.app, com.example.app.test (Wi-Fi suggestions removed with them: bench wpa2-psk)".
func packagesRemovedService(removed, suggestions []string) string {
	s := "sair: uninstalled " + strings.Join(removed, ", ")
	if len(suggestions) > 0 {
		s += " (Wi-Fi suggestions removed with them: " + strings.Join(suggestions, "; ") + ")"
	}
	return s
}

func (m *ScopedPortManager) heldByAnyPort(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sp := range m.scopedPorts {
		if _, ok := sp.Serials[serial]; ok {
			return true
		}
	}
	return false
}

// CreateScopedPort creates a scoped port for an already-acquired lock.
func (m *ScopedPortManager) CreateScopedPort(lockID string, serials map[string]struct{}, remoteDevices map[string]*pb.DeviceInfo) (*ScopedPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.scopedPorts[lockID]; exists {
		return nil, fmt.Errorf("scoped port already exists for lock %s", lockID)
	}

	// Open ephemeral port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port

	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		listener.Close()
		return nil, err
	}

	sp := &ScopedPort{
		DeviceToken:   hex.EncodeToString(token),
		LockID:        lockID,
		Serials:       serials,
		Port:          port,
		listener:      listener,
		CreatedAt:     time.Now(),
		stopCh:        make(chan struct{}),
		heartbeatStop: make(chan struct{}),
		keepaliveDone: make(chan struct{}),
		Log:           &LockLog{},
		RemoteDevices: remoteDevices,
	}

	// Start accept loop
	go m.runAcceptLoop(sp)

	// Ship logs promptly and keep the lock alive when they are quiet.
	go m.runKeepalive(sp)

	m.scopedPorts[lockID] = sp
	slog.Info("opened scoped port", "port", port, "lockId", lockID, "serials", serials)
	return sp, nil
}

// Release closes a scoped port and releases the lock on the orchestrator.
func (m *ScopedPortManager) Release(lockID, status string) bool {
	m.mu.Lock()
	sp := m.scopedPorts[lockID]
	m.mu.Unlock()
	// Close first: it stops the heartbeat (no concurrent drain) and the
	// listener (no new tunnels), so the drain below sees everything recorded.
	closed := m.CloseScopedPort(lockID)
	var entries []*pb.LockLogEntry
	if sp != nil {
		entries = sp.Log.DrainAll()
	}
	released, err := m.commandRouter.ReleaseLock(lockID, status, entries)
	if err != nil {
		slog.Warn("failed to release lock on orchestrator", "lockId", lockID, "error", err)
		return closed
	}
	return closed || released
}

// CloseScopedPort closes a scoped port without releasing the orchestrator lock.
func (m *ScopedPortManager) CloseScopedPort(lockID string) bool {
	m.mu.Lock()
	sp, exists := m.scopedPorts[lockID]
	if !exists {
		m.mu.Unlock()
		return false
	}
	delete(m.scopedPorts, lockID)
	m.mu.Unlock()

	close(sp.heartbeatStop)
	close(sp.stopCh)
	sp.listener.Close()
	<-sp.keepaliveDone // wait for runKeepalive to stop touching sp.Log before a caller drains it

	slog.Info("closed scoped port", "port", sp.Port, "lockId", lockID)
	// Every way a lock ends comes through here, so this is where a device is
	// put back to baseline: before Release hands it back to the orchestrator.
	m.resetDevices(sp)
	return true
}

// OnLockExpired is the session's callback: the orchestrator released the lock,
// so the scoped port is dead. Idempotent.
func (m *ScopedPortManager) OnLockExpired(lockID string) {
	if m.CloseScopedPort(lockID) {
		slog.Warn("lock expired on orchestrator; closed scoped port", "lockId", lockID)
	}
}

// GetAllScopedPorts returns a snapshot of all active scoped ports.
func (m *ScopedPortManager) GetAllScopedPorts() []*ScopedPort {
	m.mu.Lock()
	defer m.mu.Unlock()

	ports := make([]*ScopedPort, 0, len(m.scopedPorts))
	for _, sp := range m.scopedPorts {
		ports = append(ports, sp)
	}
	return ports
}

// ShutdownAll closes all scoped ports and releases all locks.
func (m *ScopedPortManager) ShutdownAll() {
	m.mu.Lock()
	ports := make([]*ScopedPort, 0, len(m.scopedPorts))
	for _, sp := range m.scopedPorts {
		ports = append(ports, sp)
	}
	m.mu.Unlock()

	for _, sp := range ports {
		m.CloseScopedPort(sp.LockID) // stops the heartbeat before we drain
		entries := sp.Log.DrainAll()
		if _, err := m.commandRouter.ReleaseLock(sp.LockID, "cancelled", entries); err != nil {
			slog.Warn("failed to release lock during shutdown", "lockId", sp.LockID, "error", err)
		}
	}
}

func (m *ScopedPortManager) runAcceptLoop(sp *ScopedPort) {
	slog.Debug("accept loop started", "lockId", sp.LockID, "port", sp.Port)
	for {
		conn, err := sp.listener.Accept()
		if err != nil {
			select {
			case <-sp.stopCh:
				return
			default:
				slog.Debug("accept error on scoped port", "lockId", sp.LockID, "error", err)
			}
			return
		}
		// Disable Nagle's algorithm so ADB protocol messages are sent immediately
		if tc, ok := conn.(*net.TCPConn); ok {
			if err := tc.SetNoDelay(true); err != nil {
				slog.Warn("failed to set TCP_NODELAY", "remote", conn.RemoteAddr(), "error", err)
			}
		}
		adbConn := NewAdbConnection(conn, m.commandRouter, m.deviceListTracker, sp.Serials, sp.Log, sp.RemoteDevices)
		adbConn.remoteTransportID = sp.remoteTransportID
		adbConn.remoteSerialByTransportID = sp.remoteSerialByTransportID
		go adbConn.Handle()
	}
}

// runKeepalive flushes the ADB log every flushInterval and heartbeats every
// heartbeatIntervalSecs, but only when no log was flushed in that interval:
// on the orchestrator, log traffic counts as activity.
func (m *ScopedPortManager) runKeepalive(sp *ScopedPort) {
	defer close(sp.keepaliveDone)
	flush := time.NewTicker(m.flushInterval)
	defer flush.Stop()
	beat := time.NewTicker(time.Duration(m.heartbeatIntervalSecs) * time.Second)
	defer beat.Stop()
	flushedSinceBeat := false

	for {
		select {
		case <-flush.C:
			if !m.commandRouter.sessionUp() {
				// No stream to send on (reconnecting, or unary fallback):
				// draining here would only clear the truncation flag and
				// requeue, so a job that ever hit the cap grows a fresh
				// "truncated" marker on every tick until the stream is back.
				continue
			}
			entries := sp.Log.Drain()
			if len(entries) == 0 {
				continue
			}
			if err := m.commandRouter.SendLockLog(sp.LockID, entries); err != nil {
				// Keep the entries; the next flush or the unary heartbeat ships them.
				// No stream (reconnecting, or unary fallback) is expected and quiet;
				// anything else is a send that failed on a live stream, worth seeing.
				sp.Log.Requeue(entries)
				if !errors.Is(err, errSessionDown) {
					slog.Warn("lock log flush failed; entries requeued", "lockId", sp.LockID, "entries", len(entries), "error", err)
				}
				continue
			}
			flushedSinceBeat = true
		case <-beat.C:
			if flushedSinceBeat {
				flushedSinceBeat = false
				continue
			}
			// Only drain when about to go unary: that RPC carries the log
			// atomically in the same request as the heartbeat. On the stream,
			// LockHeartbeat makes exactly one send (the heartbeat) and always
			// hands any log argument straight back as unsent, so passing nil
			// here costs nothing — and if the session flips from down to up
			// between this check and the call, LockHeartbeat's own branch still
			// only ever does one send, so there is no way to ship a log and then
			// separately fail to report it (or vice versa).
			var entries []*pb.LockLogEntry
			if !m.commandRouter.sessionUp() {
				entries = sp.Log.Drain()
			}
			alive, unsent, err := m.commandRouter.LockHeartbeat(sp.LockID, entries)
			if len(unsent) > 0 {
				sp.Log.Requeue(unsent)
			}
			if err != nil {
				if err != errSessionDown {
					slog.Warn("heartbeat failed", "lockId", sp.LockID, "error", err)
				}
				continue
			}
			if !alive {
				slog.Warn("lock expired on orchestrator; closing scoped port", "lockId", sp.LockID, "port", sp.Port)
				// Close asynchronously: CloseScopedPort waits on keepaliveDone,
				// which this goroutine only closes on return, right below.
				go m.CloseScopedPort(sp.LockID)
				return
			}
		case <-sp.heartbeatStop:
			return
		}
	}
}
