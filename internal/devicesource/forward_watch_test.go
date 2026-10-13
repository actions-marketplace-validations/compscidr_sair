package devicesource

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestStallReason(t *testing.T) {
	now := time.Unix(1000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixNano() }
	const install = "exec:cmd package install-write -S 100 1 base.apk -"
	for _, tt := range []struct {
		name, service   string
		lastIO, writing int64
		stalled         bool
	}{
		{"install-write idle", install, ago(3 * time.Minute), 0, true},
		{"install-write moving", install, ago(time.Second), 0, false},
		{"silent long test is fine", "shell,v2,raw:am instrument -w x", ago(time.Hour), 0, false},
		{"blocked write on any service", "sync:", ago(3 * time.Minute), ago(3 * time.Minute), true},
		{"write in flight briefly", "sync:", ago(time.Second), ago(time.Second), false},
	} {
		if got := stallReason(tt.service, now, tt.lastIO, tt.writing, 2*time.Minute) != ""; got != tt.stalled {
			t.Errorf("%s: stalled = %v, want %v", tt.name, got, tt.stalled)
		}
	}
}

// startForward serves a real device source whose adb server is fake: it OKAYs
// the transport and the service, then never reads or writes again, the way
// the stalled install in sair#79 looked from the outside.
func startForward(t *testing.T, service string) (pb.DeviceSource_ForwardToDeviceClient, *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			go func() {
				buf := make([]byte, 4+len("host:transport:SER"))
				c.Read(buf)
				c.Write([]byte("OKAY"))
				// Read only the service's own bytes, then go silent: no more reads.
				head := make([]byte, 4)
				c.Read(head)
				var n int
				fmt.Sscanf(string(head), "%04x", &n)
				c.Read(make([]byte, n))
				c.Write([]byte("OKAY"))
			}()
		}
	}()

	s := &Server{adbPort: ln.Addr().(*net.TCPAddr).Port, stallTimeout: 300 * time.Millisecond, idleWarnInterval: time.Hour}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterDeviceSourceServer(srv, s)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := pb.NewDeviceSourceClient(cc).ForwardToDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.ForwardData{Payload: &pb.ForwardData_Setup{Setup: &pb.ForwardSetup{Serial: "SER", InitialCommand: service}}}); err != nil {
		t.Fatal(err)
	}
	return stream, s
}

// recvUntilEnd reads the forward until it ends, or gives up after wait.
func recvUntilEnd(stream pb.DeviceSource_ForwardToDeviceClient, wait time.Duration) (error, bool) {
	end := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				end <- err
				return
			}
		}
	}()
	select {
	case err := <-end:
		return err, true
	case <-time.After(wait):
		return nil, false
	}
}

func TestStalledInstallWriteIsTornDown(t *testing.T) {
	stream, _ := startForward(t, "exec:cmd package install-write -S 100 1 base.apk -")
	stream.Send(&pb.ForwardData{Payload: &pb.ForwardData_Data{Data: []byte("first bytes")}})

	err, ended := recvUntilEnd(stream, 5*time.Second)
	if !ended {
		t.Fatal("a stalled install-write was never torn down")
	}
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "install-write") {
		t.Errorf("err = %v, want DeadlineExceeded naming the service", err)
	}
}

func TestBlockedWriteIsTornDown(t *testing.T) {
	stream, _ := startForward(t, "sync:")
	go func() {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 64; i++ { // far more than the socket buffers hold
			if stream.Send(&pb.ForwardData{Payload: &pb.ForwardData_Data{Data: chunk}}) != nil {
				return
			}
		}
	}()
	err, ended := recvUntilEnd(stream, 10*time.Second)
	if !ended {
		t.Fatal("a forward the adb server stopped reading was never torn down")
	}
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "accepted no data") {
		t.Errorf("err = %v", err)
	}
}

func TestSilentShellIsLeftAlone(t *testing.T) {
	stream, _ := startForward(t, "shell,v2,raw:am instrument -w x")
	if err, ended := recvUntilEnd(stream, 1500*time.Millisecond); ended {
		t.Errorf("a silent shell was torn down: %v", err)
	}
}

// The watch's copy of the service, which every stall reason and idle warning
// embeds, never holds a passphrase.
func TestForwardWatchStoresRedactedService(t *testing.T) {
	w := newForwardWatch("A", "shell,v2,raw:cmd wifi add-suggestion 'bench' wpa2 'hunter2'")
	if w.service != "shell,v2,raw:cmd wifi add-suggestion <redacted>" {
		t.Errorf("service = %q", w.service)
	}
	if r := stallReason(w.service, time.Now(), 0, time.Now().Add(-time.Hour).UnixNano(), time.Minute); strings.Contains(r, "hunter2") || r == "" {
		t.Errorf("stall reason = %q", r)
	}
}
