package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/permissions"
	"github.com/luthermonson/shipmates/internal/project"
)

// fleetPolicyPollInterval is how often a ship re-fetches the fleet-wide deny
// list. Kept modest so an Admiral edit reaches every ship within a few
// minutes without hammering Fleet Command. The initial fetch happens as soon
// as the fleet URL is known — no need to wait for the first tick.
const fleetPolicyPollInterval = 5 * time.Minute

// startFleetPolicy fires an initial fetch and starts a background poller that
// keeps the ship's evaluator in sync with Fleet Command's fleet-policy.yaml.
// No-op when no fleet URL is configured (a ship-only deploy has no Admiral to
// answer to). Called from Run after startFleet so fleetURL/fleetToken are set.
//
// Failure policy: on a fetch error we KEEP THE LAST-KNOWN policy in memory
// and log a warning. Failing open (drop the deny list) turns a network blip
// into a security regression; failing closed (deny everything) breaks every
// offline ship. Last-known is the reasonable middle.
func (s *Server) startFleetPolicy(ctx context.Context, conf *project.Config) {
	if conf == nil || strings.TrimSpace(conf.Fleet.URL) == "" {
		return
	}
	if s.perms == nil {
		return
	}
	go s.fleetPolicyLoop(ctx, strings.TrimRight(conf.Fleet.URL, "/"), conf.Fleet.Token())
}

func (s *Server) fleetPolicyLoop(ctx context.Context, baseURL, token string) {
	// Prime immediately so a fresh boot picks up the fleet deny list before
	// the first tool call.
	if err := s.fetchFleetPolicy(ctx, baseURL, token); err != nil {
		slog.Warn("fleet-policy: initial fetch failed, no ship-side deny yet", "err", err)
	}
	t := time.NewTicker(fleetPolicyPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
		}
		if err := s.fetchFleetPolicy(ctx, baseURL, token); err != nil {
			slog.Warn("fleet-policy: refresh failed, keeping last-known", "err", err)
			// Deliberately do NOT clear the evaluator's cached policy.
			continue
		}
	}
}

// fetchFleetPolicy pulls the current fleet deny list from Fleet Command and
// installs it on the permissions evaluator. Returns an error on any I/O or
// decode failure; the caller keeps the last-known cache in that case.
func (s *Server) fetchFleetPolicy(ctx context.Context, baseURL, token string) error {
	url := baseURL + "/api/fleet-policy"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var pol permissions.FleetPolicy
	if err := json.NewDecoder(resp.Body).Decode(&pol); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	s.perms.SetFleetPolicy(&pol)
	slog.Debug("fleet-policy: refreshed", "deny_rules", len(pol.Deny))
	return nil
}

// ErrNoFleetURL is returned by callers that need a fleet URL but the ship
// isn't wired to a fleet. Not used internally — fleet policy is silently a
// no-op in that case — but exposed for CLI diagnostics.
var ErrNoFleetURL = errors.New("no fleet URL configured")
