// Package agentmcp exposes the agent API as MCP tools: run instrumented tests in
// one call, or lease a phone and install, run shell commands, screenshot and read
// logcat on it.
package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/compscidr/sair/internal/agentapi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Intervals; variables so tests can shorten them.
var (
	pollInterval      = 5 * time.Second
	heartbeatInterval = 60 * time.Second
)

type runTestsIn struct {
	AppAPK          string `json:"app_apk" jsonschema:"path to the app APK on this machine"`
	TestAPK         string `json:"test_apk" jsonschema:"path to the instrumentation test APK on this machine"`
	Instrumentation string `json:"instrumentation,omitempty" jsonschema:"pkg/runner to run, e.g. com.example.test/androidx.test.runner.AndroidJUnitRunner; needed only when the test APK declares several"`
	Serial          string `json:"serial,omitempty" jsonschema:"a specific device; otherwise any one the API key may use"`
	TimeoutS        int    `json:"timeout_s,omitempty" jsonschema:"seconds before the run is stopped (1-3600, default 900)"`
}

type leaseIn struct {
	Serial string `json:"serial,omitempty" jsonschema:"a specific device; otherwise any one the API key may use"`
	WaitS  int    `json:"wait_s,omitempty" jsonschema:"seconds to wait for a free device (1-120, default 30)"`
}

type leaseIDIn struct {
	LeaseID string `json:"lease_id" jsonschema:"the id lease_device returned"`
}

type installIn struct {
	LeaseID string `json:"lease_id" jsonschema:"the id lease_device returned"`
	APK     string `json:"apk" jsonschema:"path to the APK on this machine"`
}

type shellIn struct {
	LeaseID  string `json:"lease_id" jsonschema:"the id lease_device returned"`
	Command  string `json:"command" jsonschema:"shell command, e.g. am start -n com.example/.MainActivity; restricted on phones shared by other users"`
	TimeoutS int    `json:"timeout_s,omitempty" jsonschema:"seconds before the command is stopped (1-600, default 60)"`
}

type logcatIn struct {
	LeaseID string `json:"lease_id" jsonschema:"the id lease_device returned"`
	Lines   int    `json:"lines,omitempty" jsonschema:"how many of the most recent lines (1-5000, default 500)"`
}

// NewServer returns an MCP server whose tools call the agent API through c.
// Leases taken through it are kept alive until released or the server stops;
// the API still ends them at the plan's maximum duration.
func NewServer(c *agentapi.Client, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "sair", Title: "SAIR", Version: version}, nil)
	k := &keeper{c: c, stop: map[string]context.CancelFunc{}}

	mcp.AddTool(s, &mcp.Tool{
		Name: "run_tests",
		Description: "Run an Android app's instrumented tests on a real phone: uploads the app and test APKs, " +
			"waits for the run to finish and returns per-test results, the instrument output and a logcat tail. " +
			"Status is passed, failed (a test failed or crashed) or error (SAIR could not run them).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runTestsIn) (*mcp.CallToolResult, any, error) {
		body, err := c.SubmitRun(ctx, in.AppAPK, in.TestAPK, in.Instrumentation, in.Serial, in.TimeoutS)
		if err != nil {
			return nil, nil, err
		}
		var sub struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &sub); err != nil || sub.ID == "" {
			return nil, nil, errors.New("no run id in the reply: " + string(body))
		}
		for {
			run, err := c.Run(ctx, sub.ID)
			if err != nil {
				return nil, nil, err
			}
			var st struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(run, &st)
			if st.Status != "queued" && st.Status != "running" {
				return text(run), nil, nil
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(pollInterval):
			}
		}
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "lease_device",
		Description: "Take a real Android phone for several calls (install_apk, shell, screenshot, logcat). " +
			"Returns the lease with its id and device. Call release_device when done: it uninstalls what the lease installed " +
			"and frees the phone. On a phone shared by another user (\"shared\": true) shell commands are restricted.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in leaseIn) (*mcp.CallToolResult, any, error) {
		body, err := c.TakeLease(ctx, in.Serial, in.WaitS)
		if err != nil {
			return nil, nil, err
		}
		var l struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &l) == nil && l.ID != "" {
			k.keep(l.ID)
		}
		return text(body), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "release_device",
		Description: "End a lease: uninstalls what it installed and frees the phone. Returns the final lease.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in leaseIDIn) (*mcp.CallToolResult, any, error) {
		k.drop(in.LeaseID)
		return reply(c.ReleaseLease(ctx, in.LeaseID))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "install_apk",
		Description: "Install an APK from this machine on the leased phone. Returns its package name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in installIn) (*mcp.CallToolResult, any, error) {
		return reply(c.Install(ctx, in.LeaseID, in.APK))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "shell",
		Description: "Run a shell command on the leased phone. Returns its output and exit code.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in shellIn) (*mcp.CallToolResult, any, error) {
		return reply(c.Shell(ctx, in.LeaseID, in.Command, in.TimeoutS))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "screenshot",
		Description: "Take a screenshot of the leased phone (PNG).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in leaseIDIn) (*mcp.CallToolResult, any, error) {
		png, err := c.Screenshot(ctx, in.LeaseID)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}}, nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "logcat",
		Description: "Read the most recent logcat lines from the leased phone.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in logcatIn) (*mcp.CallToolResult, any, error) {
		return reply(c.Logcat(ctx, in.LeaseID, in.Lines))
	})

	return s
}

func text(b []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

func reply(b []byte, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return text(b), nil, nil
}

// keeper heartbeats the leases this server took: an agent may think for longer than the
// API's 5-minute idle timeout between calls.
type keeper struct {
	c    *agentapi.Client
	mu   sync.Mutex
	stop map[string]context.CancelFunc
}

func (k *keeper) keep(id string) {
	ctx, cancel := context.WithCancel(context.Background())
	k.mu.Lock()
	k.stop[id] = cancel
	k.mu.Unlock()
	go func() {
		defer k.drop(id)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(heartbeatInterval):
			}
			var e *agentapi.Error
			// Ended (410) or gone (404): nothing left to keep alive. Other errors may pass.
			if err := k.c.Heartbeat(ctx, id); errors.As(err, &e) && (e.Status == http.StatusGone || e.Status == http.StatusNotFound) {
				return
			}
		}
	}()
}

func (k *keeper) drop(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if cancel, ok := k.stop[id]; ok {
		cancel()
		delete(k.stop, id)
	}
}
