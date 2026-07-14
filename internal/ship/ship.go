// Package ship implements the per-host supervisor: one daemon that reads
// ~/.shipmates/ship.yaml (a list of project dirs), keeps a project server alive
// in each, and restarts them on crash. It is the thing `ship install` wires
// to run at logon as a user-scoped Windows Scheduled Task or macOS launchd
// agent so project-local Codex configuration and user credentials are available.
package ship

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	projectstate "github.com/luthermonson/shipmates/internal/project"
	"gopkg.in/yaml.v3"
)

// Config is ~/.shipmates/ship.yaml.
//
// Env values pass through os.ExpandEnv — the env-indirection for creds: the
// yaml names WHERE a secret comes from (`SHIPMATES_FLEET_TOKEN: ${HOMELAB_TOKEN}`),
// never the secret itself, so the file is safe to sync between hosts.
type Config struct {
	Env      map[string]string `yaml:"env,omitempty"` // host-level, applied to every project
	Projects []Project         `yaml:"projects"`      // one coordination server per dir
}

// Project is one supervised project server plus per-project environment overrides.
type Project struct {
	Dir string            `yaml:"dir"`
	Env map[string]string `yaml:"env,omitempty"`
}

// ConfigPath is the canonical supervisor config location.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".shipmates", "ship.yaml"), nil
}

// LoadConfig reads and validates a ship.yaml. Projects whose dir doesn't
// exist are dropped with a warning rather than failing the whole ship — a
// detached external drive should not take down the other projects.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	kept := c.Projects[:0]
	for _, p := range c.Projects {
		p.Dir = strings.TrimSpace(p.Dir)
		if p.Dir == "" {
			continue
		}
		if st, err := os.Stat(p.Dir); err != nil || !st.IsDir() {
			slog.Warn("ship: project dir missing, skipping", "dir", p.Dir)
			continue
		}
		kept = append(kept, p)
	}
	c.Projects = kept
	if len(c.Projects) == 0 {
		return nil, fmt.Errorf("%s lists no usable project dirs", path)
	}
	return &c, nil
}

// env returns the merged, expanded environment for one project: the parent
// process env, then host-level entries, then per-project entries. ${VAR}
// references in values expand from the supervisor's environment.
func (c *Config) env(p Project) []string {
	out := os.Environ()
	for _, m := range []map[string]string{c.Env, p.Env} {
		for k, v := range m {
			out = append(out, k+"="+os.ExpandEnv(v))
		}
	}
	return out
}

// Restart pacing: crash-looping servers back off exponentially; a server that
// stayed up healthyReset long has proven itself and resets the ladder.
const (
	backoffMin   = time.Second
	backoffMax   = time.Minute
	healthyReset = 5 * time.Minute
	healthProbe  = 30 * time.Second // re-check cadence when an unmanaged server already runs
)

// Run supervises every configured project until ctx is cancelled. Blocks.
func Run(ctx context.Context, c *Config) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	var wg sync.WaitGroup
	for _, p := range c.Projects {
		wg.Add(1)
		go func(p Project) {
			defer wg.Done()
			superviseLoop(ctx, exe, p, c.env(p))
		}(p)
	}
	slog.Info("ship supervising", "projects", len(c.Projects))
	wg.Wait()
	return nil
}

// superviseLoop keeps one project's coordination server alive: spawn, wait, restart
// with backoff. If a healthy server is already running in the dir (started
// manually, or by a previous supervisor), it stands by and re-probes instead
// of racing it — two servers in one project would fight over the port file.
func superviseLoop(ctx context.Context, exe string, p Project, env []string) {
	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		if serverHealthy(p.Dir) {
			slog.Info("ship: project server already running, standing by", "dir", p.Dir)
			if !sleepCtx(ctx, healthProbe) {
				return
			}
			continue
		}

		started := time.Now()
		err := runCaptain(ctx, exe, p.Dir, env)
		if ctx.Err() != nil {
			return
		}
		uptime := time.Since(started)
		if uptime >= healthyReset {
			backoff = backoffMin
		}
		slog.Warn("ship: project server exited, restarting", "dir", p.Dir, "uptime", uptime.Round(time.Second), "backoff", backoff, "err", err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// runCaptain spawns `shipmates server serve` in the project dir and waits for it
// to exit. Its output appends to the project's .shipmates/sessions/server.log.
// On ctx cancel we ask the server to shut down gracefully (it reaps its crew
// processes) before falling back to a hard kill.
func runCaptain(ctx context.Context, exe, dir string, env []string) error {
	cmd := exec.CommandContext(ctx, exe, "server", "serve")
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error {
		if shutdownServer(dir) {
			return nil // graceful path accepted; WaitDelay below is the backstop
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 10 * time.Second

	logPath := filepath.Join(dir, ".shipmates", "sessions", "server.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			defer f.Close()
			cmd.Stdout = f
			cmd.Stderr = f
		}
	}
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}

	slog.Info("ship: starting project server", "dir", dir)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

// serverPort reads the project's recorded coordination-server port, 0 when absent.
func serverPort(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, ".shipmates", "sessions", "server.port"))
	if err != nil {
		return 0
	}
	var port int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &port)
	return port
}

// serverHealthy probes the project's recorded server port. A stale port file
// (server crashed without cleanup) fails the probe and is treated as absent.
func serverHealthy(dir string) bool {
	port := serverPort(dir)
	if port == 0 {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	scope, err := projectstate.ScopeID(dir)
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Shipmates-Project", scope)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && resp.Header.Get("X-Shipmates-Project") == scope
}

// shutdownServer asks the project's coordination server to exit gracefully.
func shutdownServer(dir string) bool {
	port := serverPort(dir)
	if port == 0 {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	scope, err := projectstate.ScopeID(dir)
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/shutdown", port), nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Shipmates-Project", scope)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 300
}

// sleepCtx sleeps for d, returning false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// AddProject appends a project dir to the config at path, creating the file
// when absent. Note: rewrites the yaml, so hand-written comments don't
// survive — the file is meant to be machine-managed via this command.
func AddProject(path, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.ToSlash(abs)
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}

	var c Config
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, p := range c.Projects {
		if strings.EqualFold(filepath.ToSlash(strings.TrimSpace(p.Dir)), abs) {
			return fmt.Errorf("%s is already in %s", abs, path)
		}
	}
	c.Projects = append(c.Projects, Project{Dir: abs})

	out, err := yaml.Marshal(&c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// ProjectStatus is one supervised project's probe result.
type ProjectStatus struct {
	Dir     string
	Running bool
	Port    int
	PID     int
}

// StatusAll probes every configured project's recorded coordination-server port.
func StatusAll(c *Config) []ProjectStatus {
	out := make([]ProjectStatus, 0, len(c.Projects))
	for _, p := range c.Projects {
		st := ProjectStatus{Dir: p.Dir}
		st.Port = serverPort(p.Dir)
		if b, err := os.ReadFile(filepath.Join(p.Dir, ".shipmates", "sessions", "server.pid")); err == nil {
			_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &st.PID)
		}
		st.Running = serverHealthy(p.Dir)
		out = append(out, st)
	}
	return out
}

// ErrUnsupported marks install/uninstall on platforms without an implementation.
var ErrUnsupported = errors.New("ship install is implemented for windows (Scheduled Task) and darwin (launchd user agent) only")
