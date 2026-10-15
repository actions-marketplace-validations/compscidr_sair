// Package agentapi is a client for the orchestrator's agent API: one-call test
// runs (/api/v1/runs) and interactive device leases (/api/v1/leases).
package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Client talks to the agent API at BaseURL with an API key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Error is an answer other than 2xx: the HTTP status and the API's message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// SubmitRun uploads an app and a test APK and starts a run; the reply is {"id": …}.
func (c *Client) SubmitRun(ctx context.Context, appAPK, testAPK, instrumentation, serial string, timeoutS int) ([]byte, error) {
	fields := url.Values{}
	setIf(fields, "instrumentation", instrumentation)
	setIf(fields, "serial", serial)
	if timeoutS > 0 {
		fields.Set("timeout_s", strconv.Itoa(timeoutS))
	}
	return c.upload(ctx, "/api/v1/runs", [][2]string{{"app", appAPK}, {"test", testAPK}}, fields)
}

// Run is a run's state: status (queued, running, passed, failed, error), tests, logcat, ….
func (c *Client) Run(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id), nil, "")
}

// TakeLease takes a device, waiting up to waitS seconds for one (0: the server's default).
func (c *Client) TakeLease(ctx context.Context, serial string, waitS int) ([]byte, error) {
	fields := url.Values{}
	setIf(fields, "serial", serial)
	if waitS > 0 {
		fields.Set("wait_s", strconv.Itoa(waitS))
	}
	return c.form(ctx, "/api/v1/leases", fields)
}

func (c *Client) Lease(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, leasePath(id, ""), nil, "")
}

// ReleaseLease ends the lease: what it installed is uninstalled and the device freed.
func (c *Client) ReleaseLease(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodDelete, leasePath(id, ""), nil, "")
}

func (c *Client) Heartbeat(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, leasePath(id, "/heartbeat"), nil, "")
	return err
}

// Install uploads an APK to the leased device; the reply is {"package": …}.
func (c *Client) Install(ctx context.Context, id, apk string) ([]byte, error) {
	return c.upload(ctx, leasePath(id, "/install"), [][2]string{{"apk", apk}}, nil)
}

// Shell runs a command; the reply is {"output": …, "exit_code": …}.
func (c *Client) Shell(ctx context.Context, id, command string, timeoutS int) ([]byte, error) {
	fields := url.Values{"command": {command}}
	if timeoutS > 0 {
		fields.Set("timeout_s", strconv.Itoa(timeoutS))
	}
	return c.form(ctx, leasePath(id, "/shell"), fields)
}

// Screenshot is a PNG of the leased device's screen.
func (c *Client) Screenshot(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, leasePath(id, "/screenshot"), nil, "")
}

func (c *Client) Logcat(ctx context.Context, id string, lines int) ([]byte, error) {
	path := leasePath(id, "/logcat")
	if lines > 0 {
		path += "?lines=" + strconv.Itoa(lines)
	}
	return c.do(ctx, http.MethodGet, path, nil, "")
}

func leasePath(id, rest string) string { return "/api/v1/leases/" + url.PathEscape(id) + rest }

func setIf(v url.Values, k, s string) {
	if s != "" {
		v.Set(k, s)
	}
}

func (c *Client) form(ctx context.Context, path string, fields url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, strings.NewReader(fields.Encode()), "application/x-www-form-urlencoded")
}

// upload streams files (part name, local path) and fields as multipart/form-data, without buffering
// the APKs (up to 200 MB each) in memory.
func (c *Client) upload(ctx context.Context, path string, files [][2]string, fields url.Values) ([]byte, error) {
	for _, f := range files {
		if _, err := os.Stat(f[1]); err != nil {
			return nil, err
		}
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for k, vs := range fields {
				for _, v := range vs {
					if err := mw.WriteField(k, v); err != nil {
						return err
					}
				}
			}
			for _, f := range files {
				if err := copyFile(mw, f[0], f[1]); err != nil {
					return err
				}
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	body, err := c.do(ctx, http.MethodPost, path, pr, mw.FormDataContentType())
	pr.Close() // unblocks the writer if the request ended before reading it all
	return body, err
}

func copyFile(mw *multipart.Writer, part, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := mw.CreateFormFile(part, filepath.Base(path))
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &Error{Status: resp.StatusCode, Message: msg}
	}
	return data, nil
}
