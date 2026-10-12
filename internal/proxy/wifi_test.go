package proxy

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	dspb "github.com/compscidr/sair/proto/devicesource"
	pb "github.com/compscidr/sair/proto/orchestrator"
)

func TestScopedPortWifi(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var calls []string
	m.setWifi = func(serial string, state dspb.WifiRequest_State) error {
		calls = append(calls, serial+" "+state.String())
		return nil
	}
	m.removeReverse = func(serial string) error {
		calls = append(calls, serial+" unreverse")
		return nil
	}
	set := func(s ...string) map[string]struct{} {
		out := map[string]struct{}{}
		for _, v := range s {
			out[v] = struct{}{}
		}
		return out
	}
	if _, err := m.CreateScopedPort("L1", set("A", "R"), map[string]*pb.DeviceInfo{"R": {Serial: "R"}}); err != nil {
		t.Fatal(err)
	}

	// "All" skips the relayed device; naming a serial outside the lock is refused.
	if err := m.SetWifi("L1", nil, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetWifi("L1", []string{"B"}, false); !errors.Is(err, errNotInLock) {
		t.Errorf("serial outside lock: err = %v", err)
	}
	if err := m.SetWifi("nope", nil, false); !errors.Is(err, errUnknownLock) {
		t.Errorf("unknown lock: err = %v", err)
	}

	// L1 expired and L2 already holds A: closing L1 must not reset under L2.
	if _, err := m.CreateScopedPort("L2", set("A"), nil); err != nil {
		t.Fatal(err)
	}
	m.CloseScopedPort("L1")
	m.CloseScopedPort("L2")

	want := []string{"A ON", "A unreverse", "A RESET"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// Two phones in one lock change state in parallel: each setWifi waits at a
// barrier for the other to arrive, which only clears when the calls overlap
// (sair#84). The same phone still serializes, so ON then OFF stays ordered.
func TestScopedPortWifiFansOutPerDevice(t *testing.T) {
	m := NewScopedPortManager(&CommandRouter{}, nil, 3600)
	var barrier sync.WaitGroup
	barrier.Add(2)
	m.setWifi = func(serial string, state dspb.WifiRequest_State) error {
		barrier.Done()
		barrier.Wait()
		return nil
	}
	if _, err := m.CreateScopedPort("L1", map[string]struct{}{"A": {}, "B": {}}, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.SetWifi("L1", nil, true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetWifi serialized the two devices")
	}

	// Same serial: the second call must wait for the first.
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex
	m.setWifi = func(serial string, state dspb.WifiRequest_State) error {
		if state == dspb.WifiRequest_ON {
			<-release
		}
		mu.Lock()
		order = append(order, state.String())
		mu.Unlock()
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- m.SetWifi("L1", []string{"A"}, true) }()
	time.Sleep(50 * time.Millisecond) // let the ON take A's lock before the OFF asks for it
	second := make(chan error, 1)
	go func() { second <- m.SetWifi("L1", []string{"A"}, false) }()
	select {
	case <-second:
		t.Fatal("OFF on A ran while ON on A was still in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-first
	<-second
	if want := []string{"ON", "OFF"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}
