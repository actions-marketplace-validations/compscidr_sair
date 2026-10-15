package agentmcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compscidr/sair/internal/agentapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAPI answers like the orchestrator's agent API and records what it was sent.
type fakeAPI struct {
	mu         sync.Mutex
	polls      int
	uploads    map[string]string // part name -> content
	shell      string
	heartbeats int
	released   bool
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer k" {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"invalid key"}`)
		return
	}
	switch {
	case r.Method == "POST" && (r.URL.Path == "/api/v1/runs" || r.URL.Path == "/api/v1/leases/L1/install"):
		mr, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(415)
			return
		}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			f.uploads[p.FormName()] = string(b)
		}
		if r.URL.Path == "/api/v1/runs" {
			io.WriteString(w, `{"id":"R1"}`)
		} else {
			io.WriteString(w, `{"package":"com.example"}`)
		}
	case r.URL.Path == "/api/v1/runs/R1":
		f.polls++
		if f.polls < 3 {
			io.WriteString(w, `{"status":"running","id":"R1"}`)
		} else {
			io.WriteString(w, `{"status":"passed","id":"R1"}`)
		}
	case r.URL.Path == "/api/v1/devices":
		io.WriteString(w, `{"devices":[{"serial":"B1","model":"Pixel 8","sdk":34,"shared":false,"in_use":false}]}`)
	case r.Method == "POST" && r.URL.Path == "/api/v1/leases":
		w.WriteHeader(201)
		io.WriteString(w, `{"status":"active","id":"L1","device":"B1","shared":true}`)
	case r.URL.Path == "/api/v1/leases/L1/heartbeat":
		f.heartbeats++
		w.WriteHeader(204)
	case r.URL.Path == "/api/v1/leases/L1/shell":
		r.ParseForm()
		f.shell = r.PostForm.Get("command")
		if f.shell == "reboot" {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":"'reboot' is not allowed on a shared device"}`)
			return
		}
		io.WriteString(w, `{"output":"hi\n","exit_code":0}`)
	case r.URL.Path == "/api/v1/leases/L1/screenshot":
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG\r\n\x1a\n"))
	case r.URL.Path == "/api/v1/leases/L1/logcat":
		io.WriteString(w, "lines="+r.URL.Query().Get("lines"))
	case r.Method == "DELETE" && r.URL.Path == "/api/v1/leases/L1":
		f.released = true
		io.WriteString(w, `{"status":"ended","id":"L1"}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"unknown"}`)
	}
}

func (f *fakeAPI) get(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func TestMain(m *testing.M) {
	// Set once: heartbeat goroutines of earlier tests may still read them.
	pollInterval, heartbeatInterval = 10*time.Millisecond, 20*time.Millisecond
	os.Exit(m.Run())
}

func connect(t *testing.T, key string) (*mcp.ClientSession, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{uploads: map[string]string{}}
	hs := httptest.NewServer(api)
	t.Cleanup(hs.Close)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := NewServer(&agentapi.Client{BaseURL: hs.URL, APIKey: key}, "test").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, api
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, not text", res.Content[0])
	}
	return tc.Text
}

func apk(t *testing.T, name, content string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunTestsUploadsBothAPKsAndWaitsForTheResult(t *testing.T) {
	cs, api := connect(t, "k")
	res := call(t, cs, "run_tests", map[string]any{"app_apk": apk(t, "app.apk", "APP"), "test_apk": apk(t, "test.apk", "TEST")})
	if res.IsError || textOf(t, res) != `{"status":"passed","id":"R1"}` {
		t.Fatalf("got %+v", textOf(t, res))
	}
	api.get(func() {
		if api.uploads["app"] != "APP" || api.uploads["test"] != "TEST" {
			t.Errorf("uploads = %v", api.uploads)
		}
		if api.polls != 3 {
			t.Errorf("polled %d times, want 3", api.polls)
		}
	})
}

func TestLeaseInstallShellScreenshotLogcatRelease(t *testing.T) {
	cs, api := connect(t, "k")
	if got := textOf(t, call(t, cs, "list_devices", nil)); !strings.Contains(got, `"serial":"B1"`) {
		t.Fatalf("list_devices = %s", got)
	}
	if got := textOf(t, call(t, cs, "lease_device", nil)); !strings.Contains(got, `"id":"L1"`) {
		t.Fatalf("lease_device = %s", got)
	}
	if got := textOf(t, call(t, cs, "install_apk", map[string]any{"lease_id": "L1", "apk": apk(t, "app.apk", "APP")})); got != `{"package":"com.example"}` {
		t.Errorf("install_apk = %s", got)
	}
	if got := textOf(t, call(t, cs, "shell", map[string]any{"lease_id": "L1", "command": "echo hi"})); got != `{"output":"hi\n","exit_code":0}` {
		t.Errorf("shell = %s", got)
	}
	shot := call(t, cs, "screenshot", map[string]any{"lease_id": "L1"})
	if img, ok := shot.Content[0].(*mcp.ImageContent); !ok || img.MIMEType != "image/png" || string(img.Data) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("screenshot = %+v", shot.Content[0])
	}
	if got := textOf(t, call(t, cs, "logcat", map[string]any{"lease_id": "L1", "lines": 20})); got != "lines=20" {
		t.Errorf("logcat = %s", got)
	}
	time.Sleep(100 * time.Millisecond) // several heartbeat intervals
	call(t, cs, "release_device", map[string]any{"lease_id": "L1"})
	var beats int
	api.get(func() {
		beats = api.heartbeats
		if api.uploads["apk"] != "APP" || !api.released {
			t.Errorf("uploads = %v, released = %v", api.uploads, api.released)
		}
	})
	if beats == 0 {
		t.Error("the lease was never heartbeated")
	}
	time.Sleep(100 * time.Millisecond)
	api.get(func() {
		if api.heartbeats != beats {
			t.Errorf("heartbeats went on after release: %d -> %d", beats, api.heartbeats)
		}
	})
}

func TestAPIRefusalsAreToolErrorsWithTheAPIsMessage(t *testing.T) {
	cs, _ := connect(t, "k")
	res := call(t, cs, "shell", map[string]any{"lease_id": "L1", "command": "reboot"})
	if !res.IsError || !strings.Contains(textOf(t, res), "403") || !strings.Contains(textOf(t, res), "not allowed on a shared device") {
		t.Errorf("got IsError=%v %q", res.IsError, textOf(t, res))
	}
	bad, _ := connect(t, "wrong")
	res = call(t, bad, "lease_device", nil)
	if !res.IsError || !strings.Contains(textOf(t, res), "invalid key") {
		t.Errorf("got IsError=%v %q", res.IsError, textOf(t, res))
	}
	res = call(t, cs, "install_apk", map[string]any{"lease_id": "L1", "apk": "/nonexistent.apk"})
	if !res.IsError || !strings.Contains(textOf(t, res), "nonexistent.apk") {
		t.Errorf("got IsError=%v %q", res.IsError, textOf(t, res))
	}
}
