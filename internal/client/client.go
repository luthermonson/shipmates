// Package client lets CLI commands talk to the captain-spawned coordination
// server, auto-starting it (detached) when it isn't already running.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

// AttachResp mirrors the JSON returned by the ship's POST /attach — kept in
// the client package to avoid pulling internal/server into every CLI caller.
type AttachResp struct {
	AttachID string `json:"attachId"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

func port() (int, error) {
	b, err := os.ReadFile(project.PortFile())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func base() (string, error) {
	p, err := port()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://127.0.0.1:%d", p), nil
}

// Healthy reports whether a server is reachable.
func Healthy() bool {
	b, err := base()
	if err != nil {
		return false
	}
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(b + "/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		return err
	}
	logf, err := os.Create(project.LogFile())
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
	b, err := base()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	resp, err := http.Post(b+path, "application/json", &buf)
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
	b, err := base()
	if err != nil {
		return nil, err
	}
	resp, err := http.Get(b + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// PostAttach uploads a file to the server's /attach endpoint as multipart
// form-data with an optional caption, returning the parsed AttachResp. Used
// by `shipmates show` — the CLI file-attach counterpart to Tell.
func PostAttach(filePath, caption string) (*AttachResp, error) {
	b, err := base()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filePath, err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	resp, err := http.Post(b+"/attach", w.FormDataContentType(), &buf)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out AttachResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &out, nil
}
