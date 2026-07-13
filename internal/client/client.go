// Package client lets CLI commands talk to the captain-spawned coordination
// server, auto-starting it (detached) when it isn't already running.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

type endpoint struct {
	base, scope, token string
}

func discover() (endpoint, error) {
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return endpoint{}, errors.New("local server unavailable")
	}
	scope, err := project.ScopeID(root)
	if err != nil {
		return endpoint{}, errors.New("local server unavailable")
	}
	b, err := project.ReadServerStateFile(root, "server.json", 4096)
	if err != nil {
		return endpoint{}, errors.New("local server unavailable")
	}
	var record struct {
		SchemaVersion uint64 `json:"schema_version"`
		ProjectRoot   string `json:"project_root"`
		ProjectScope  string `json:"project_scope"`
		Address       string `json:"address"`
		PID           int    `json:"pid"`
		ControlToken  string `json:"control_token"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&record) != nil || dec.Decode(&struct{}{}) != io.EOF || record.SchemaVersion != 1 || record.ProjectRoot != root || record.ProjectScope != scope || record.PID < 1 || len(record.ControlToken) < 32 || strings.ContainsAny(record.ControlToken, " \t\r\n") {
		return endpoint{}, errors.New("local server unavailable")
	}
	host, port, err := net.SplitHostPort(record.Address)
	if err != nil || port == "" {
		return endpoint{}, errors.New("local server unavailable")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return endpoint{}, errors.New("local server unavailable")
	}
	alive, err := project.ProcessAlive(record.PID)
	if err != nil || !alive {
		return endpoint{}, errors.New("local server unavailable")
	}
	return endpoint{base: "http://" + net.JoinHostPort(host, port), scope: scope, token: record.ControlToken}, nil
}

const projectHeader = "X-Shipmates-Project"

var localControlHTTPClient = &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
	req.Header.Del("Authorization")
	req.Body = nil
	return http.ErrUseLastResponse
}}

// Healthy reports whether a server is reachable.
func Healthy() bool {
	ep, err := discover()
	if err != nil {
		return false
	}
	c := http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequest(http.MethodGet, ep.base+"/health", nil)
	if err != nil {
		return false
	}
	req.Header.Set(projectHeader, ep.scope)
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK && resp.Header.Get(projectHeader) == ep.scope
}

// EnsureRunning starts the server detached if it isn't healthy, and waits for it.
func EnsureRunning() error {
	if Healthy() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return err
	}
	if err := project.EnsureServerStateDirectory(root); err != nil {
		return err
	}
	logf, err := project.OpenServerLog(root)
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "server", "serve")
	cmd.Stdout = logf
	cmd.Stderr = logf
	detach(cmd) // platform-specific: new process group / detached
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	for i := 0; i < 50; i++ {
		if Healthy() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server did not become healthy (see %s)", project.LogFile())
}

// Post sends a JSON body to a server path.
func Post(path string, body any) ([]byte, error) {
	ep, err := discover()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(http.MethodPost, ep.base+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	authorize(req, ep, path)
	resp, err := clientForPath(path).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Get fetches a server path.
func Get(path string) ([]byte, error) {
	ep, err := discover()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, ep.base+path, nil)
	if err != nil {
		return nil, err
	}
	authorize(req, ep, path)
	resp, err := clientForPath(path).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Do performs a context-bound local request. The returned response body is
// owned by the caller, which permits bounded feed following without buffering.
func Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	ep, err := discover()
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
		r = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, ep.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	authorize(req, ep, path)
	return clientForPath(path).Do(req)
}

func authorize(req *http.Request, ep endpoint, path string) {
	req.Header.Set(projectHeader, ep.scope)
	if path == "/shutdown" || strings.HasPrefix(path, "/api/local/v1/") {
		req.Header.Set("Authorization", "Bearer "+ep.token)
	}
}

func clientForPath(path string) *http.Client {
	if path == "/shutdown" || strings.HasPrefix(path, "/api/local/v1/") {
		return localControlHTTPClient
	}
	return http.DefaultClient
}
