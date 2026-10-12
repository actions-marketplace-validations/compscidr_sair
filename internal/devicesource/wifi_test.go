package devicesource

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/compscidr/sair/proto/devicesource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeShell records commands and fails any whose prefix is in fail.
func fakeShell(s *Server, outputs map[string]string, fail ...string) *[]string {
	var cmds []string
	s.shell = func(_ context.Context, _, command string) (string, error) {
		cmds = append(cmds, command)
		for _, f := range fail {
			if strings.HasPrefix(command, f) {
				return "", errors.New("exit status 1")
			}
		}
		return outputs[command], nil
	}
	return &cmds
}

const (
	statusConnected    = "Wifi is enabled\nWifi scanning is always available\nWifi is connected to \"it's here\"\nWifiInfo: SSID: \"it's here\", BSSID: 00:11:22:33:44:55\n"
	statusNotConnected = "Wifi is enabled\nWifi scanning is always available\nWifi is not connected\n"
	statusDisabled     = "Wifi is disabled\nWifi scanning is only available when wifi is enabled\n"
	connectivityWifi   = "Active default network: 100\nCurrent Networks:\n  NetworkAgentInfo{ ni{[type: WIFI[], state: CONNECTED/CONNECTED]}  network{100}  nc{[ Transports: WIFI Capabilities: NOT_METERED&INTERNET&VALIDATED ]} }\n"
)

func TestSetWifi(t *testing.T) {
	listing := "Network Id      SSID                         Security type\n0            bench                          wpa2-psk\n3            other                          open\n"
	forget := []string{"cmd wifi remove-all-suggestions", "cmd wifi list-networks", "cmd wifi forget-network 0", "cmd wifi forget-network 3"}
	tests := []struct {
		name   string
		state  pb.WifiRequest_State
		status string
		fail   []string
		want   []string
	}{
		// OFF and RESET end once the phone is not associated and the ping fails.
		{"reset", pb.WifiRequest_RESET, statusNotConnected, []string{"ping"}, append(slices.Clone(forget),
			"svc wifi disable", "svc wifi enable", "cmd wifi status", "ping -c1 -W2 'dns.google'",
		)},
		{"off", pb.WifiRequest_OFF, statusNotConnected, []string{"ping"}, append(slices.Clone(forget),
			"cmd wifi status", "ping -c1 -W2 'dns.google'",
		)},
		{"off falls back to the bench SSID", pb.WifiRequest_OFF, statusNotConnected, []string{"cmd wifi remove-all", "ping"}, []string{
			"cmd wifi remove-all-suggestions", `cmd wifi remove-suggestion 'it'\''s here'`, "cmd wifi list-networks",
			"cmd wifi forget-network 0", "cmd wifi forget-network 3", "cmd wifi status", "ping -c1 -W2 'dns.google'",
		}},
		// ON ends once associated to the bench SSID, validated, and the ping answers.
		{"on", pb.WifiRequest_ON, statusConnected, nil, []string{
			`cmd wifi add-suggestion 'it'\''s here' wpa2 'pass word'`, "cmd wifi start-scan",
			"cmd wifi status", "dumpsys connectivity", "ping -c1 -W2 'dns.google'",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{wifiSSID: "it's here", wifiPassphrase: "pass word", wifiPingHost: "dns.google", wifiTimeout: time.Second, wifiOffTimeout: time.Second}
			cmds := fakeShell(s, map[string]string{
				"cmd wifi list-networks": listing,
				"cmd wifi status":        tt.status,
				"dumpsys connectivity":   connectivityWifi,
			}, tt.fail...)
			if _, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: tt.state}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*cmds, tt.want) {
				t.Errorf("commands:\n got %q\nwant %q", *cmds, tt.want)
			}
		})
	}
}

// Every way a phone can fail the online or offline contract is named in the
// DeadlineExceeded error, so an AP or leaked-suggestion problem is read from
// the acquire log rather than from a red test (sair#86, sair#87).
func TestSetWifiReportsWhyTheContractFailed(t *testing.T) {
	tests := []struct {
		name         string
		state        pb.WifiRequest_State
		status       string
		connectivity string
		fail         []string
		want         string
	}{
		{"on: radio off", pb.WifiRequest_ON, statusDisabled, connectivityWifi, nil, "Wi-Fi radio is off"},
		{"on: not associated", pb.WifiRequest_ON, statusNotConnected, connectivityWifi, nil, "not associated to any network"},
		{"on: wrong network", pb.WifiRequest_ON, strings.Replace(statusConnected, "it's here", "Cafe", 1), connectivityWifi, nil, `associated to "Cafe", not "it's here"`},
		{"on: no Wi-Fi network", pb.WifiRequest_ON, statusConnected, "Active default network: none\n", nil, "no Wi-Fi network"},
		{"on: default is cellular", pb.WifiRequest_ON, statusConnected, "Active default network: 101\n" + strings.Replace(connectivityWifi, "Active default network: 100\n", "", 1) + "  NetworkAgentInfo{ ni{[type: MOBILE[]]}  network{101}  nc{[ Transports: CELLULAR Capabilities: INTERNET&VALIDATED ]} }\n", nil, "default network is cellular"},
		{"on: not validated", pb.WifiRequest_ON, statusConnected, strings.Replace(connectivityWifi, "&VALIDATED", "", 1), nil, "connected but not VALIDATED"},
		{"on: ping fails", pb.WifiRequest_ON, statusConnected, connectivityWifi, []string{"ping"}, "cannot reach dns.google"},
		{"off: still on the bench network", pb.WifiRequest_OFF, statusConnected, "", []string{"ping"}, `still associated to "it's here"`},
		{"off: still has a route", pb.WifiRequest_OFF, statusNotConnected, "", nil, "still reaches dns.google (mobile data?)"},
		{"off: still has a route over another network", pb.WifiRequest_OFF, strings.Replace(statusConnected, "it's here", "Cafe", 2), "", nil, `still reaches dns.google over Wi-Fi "Cafe"`},
		{"reset: radio stays off", pb.WifiRequest_RESET, statusDisabled, "", []string{"ping"}, "radio is still off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{wifiSSID: "it's here", wifiPingHost: "dns.google", wifiTimeout: time.Millisecond, wifiOffTimeout: time.Millisecond}
			fakeShell(s, map[string]string{"cmd wifi status": tt.status, "dumpsys connectivity": tt.connectivity}, tt.fail...)
			_, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: tt.state})
			if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want DeadlineExceeded containing %q", err, tt.want)
			}
		})
	}
}

// A VPN on top of the bench Wi-Fi (the app under test may be one) is still
// online; a Wi-Fi Direct group with no uplink is still offline (sair#88 review).
func TestSetWifiToleratesVpnAndDirectGroups(t *testing.T) {
	vpn := "Active default network: 102\nCurrent Networks:\n" +
		"  NetworkAgentInfo{ ni{[type: VPN[]]}  network{102}  nc{[ Transports: VPN Capabilities: INTERNET&VALIDATED ]} }\n" +
		"  NetworkAgentInfo{ ni{[type: WIFI[]]}  network{100}  nc{[ Transports: WIFI Capabilities: INTERNET&VALIDATED ]} }\n"
	s := &Server{wifiSSID: "it's here", wifiPingHost: "dns.google", wifiTimeout: time.Millisecond, wifiOffTimeout: time.Millisecond}
	fakeShell(s, map[string]string{"cmd wifi status": statusConnected, "dumpsys connectivity": vpn})
	if _, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: pb.WifiRequest_ON}); err != nil {
		t.Errorf("ON under a VPN: %v", err)
	}
	fakeShell(s, map[string]string{"cmd wifi status": strings.Replace(statusConnected, "it's here", "DIRECT-TEST-ab", 2)}, "ping")
	if _, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: pb.WifiRequest_OFF}); err != nil {
		t.Errorf("OFF on a Wi-Fi Direct group without uplink: %v", err)
	}
}

func TestSetWifiOnTimesOutWithoutInternet(t *testing.T) {
	s := &Server{wifiSSID: "bench", wifiPingHost: "dns.google", wifiTimeout: time.Millisecond}
	cmds := fakeShell(s, map[string]string{"cmd wifi status": strings.Replace(statusConnected, "it's here", "bench", 2), "dumpsys connectivity": connectivityWifi}, "ping")
	_, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: pb.WifiRequest_ON})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if (*cmds)[0] != "cmd wifi add-suggestion 'bench' open" {
		t.Errorf("no passphrase should suggest an open network, got %q", (*cmds)[0])
	}
}

func TestSetWifiUnmanagedWithoutSSID(t *testing.T) {
	s := &Server{}
	cmds := fakeShell(s, nil)
	_, err := s.SetWifi(context.Background(), &pb.WifiRequest{Serial: "ABC", State: pb.WifiRequest_RESET})
	if status.Code(err) != codes.FailedPrecondition || len(*cmds) != 0 {
		t.Fatalf("err = %v, cmds = %q; want FailedPrecondition and no commands", err, *cmds)
	}
}
