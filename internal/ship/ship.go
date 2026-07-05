// Package ship implements the per-host supervisor: one daemon that reads
// ~/.shipmates/ship.yaml (a list of project dirs), keeps a lead server alive
// in each, and restarts them on crash. It is the thing `ship install` wires
// to run at logon (Windows Scheduled Task / macOS launchd user agent — NOT a
// session-0 service: claude needs the user's environment and credentials).
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

	"gopkg.in/yaml.v3"
)

// Config is ~/.shipmates/ship.yaml.
//
// Env values pass through os.ExpandEnv — the env-indirection for creds: the
// yaml names WHERE a secret comes from (`SHIPMATES_BRIDGE_TOKEN: ${HOMELAB_TOKEN}`),
// never the secret itself, so the file is safe to sync between hosts.
type Config struct {
	Env      map[string]string `yaml:"env"`      // host-level, applied to every lead
	Projects []Project         `yaml:"projects"` // one lead server per dir
}

// Project is one supervised lead: a project dir plus per-project env overrides.
type Project struct {
	Dir string            `yaml:"dir"`
	Env map[string]string `yaml:"env"`
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
// detached external drive shouldn't take down the other leads.
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

// Restart pacing: crash-looping leads back off exponentially; a lead that
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

// superviseLoop keeps one project's lead server alive: spawn, wait, restart
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
			slog.Info("ship: lead already running, standing by", "dir", p.Dir)
			if !sleepCtx(ctx, healthProbe) {
				return
			}
			continue
		}

		started := time.Now()
		err := runLead(ctx, exe, p.Dir, env)
		if ctx.Err() != nil {
			return
		}
		uptime := time.Since(started)
		if uptime >= healthyReset {
			backoff = backoffMin
		}
		slog.Warn("ship: lead exited, restarting", "dir", p.Dir, "uptime", uptime.Round(time.Second), "backoff", backoff, "err", err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// runLead spawns `shipmates server serve` in the project dir and waits for it
// to exit. Its output appends to the project's .shipmates/sessions/server.log.
// On ctx cancel we ask the server to shut down gracefully (it reaps its crew
// processes) before falling back to a hard kill.
func runLead(ctx context.Context, exe, dir string, env []string) error {
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

	slog.Info("ship: starting lead", "dir", dir)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

// serverPort reads the project's recorded lead-server port, 0 when absent.
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
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// shutdownServer asks the project's lead server to exit gracefully.
func shutdownServer(dir string) bool {
	port := serverPort(dir)
	if port == 0 {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/shutdown", port), "application/json", nil)
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

// ErrUnsupported marks install/uninstall on platforms without an implementation.
var ErrUnsupported = errors.New("ship install is implemented for windows (Scheduled Task) and darwin (launchd user agent) only")
