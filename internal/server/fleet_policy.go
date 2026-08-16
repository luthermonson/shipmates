package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/fleeturl"
	"github.com/luthermonson/shipmates/internal/permissions"
	"github.com/luthermonson/shipmates/internal/project"
)

// fleetPolicyPollInterval is how often a ship re-fetches the fleet-wide deny
// list. Kept modest so an Admiral edit reaches every ship within a few
// minutes without hammering Fleet Command. The initial fetch happens as soon
// as the fleet URL is known — no need to wait for the first tick.
const fleetPolicyPollInterval = 5 * time.Minute

// fleetPolicyRetryInterval is the (much shorter) interval used while the ship
// has NO fleet policy in force at all — neither a fresh fetch nor a cached
// one. That is the only genuinely fail-open state left, so we leave it as
// fast as we can without hammering: minutes of unrestricted operation because
// Fleet Command was rebooting is exactly the hole this file exists to close.
const fleetPolicyRetryInterval = 15 * time.Second

// maxFleetPolicyBytes caps the response we will read from /api/fleet-policy.
// A fleet deny list is a few dozen rule strings; anything approaching a
// megabyte is a broken (or hostile) endpoint, and decoding it unbounded would
// hand a remote peer the ship's memory.
const maxFleetPolicyBytes = 1 << 20 // 1 MiB

// fleetPolicyCacheName is the on-disk copy of the last policy Fleet Command
// successfully handed down. See fleetPolicyCachePath.
const fleetPolicyCacheName = "fleet-policy.json"

// fleetPolicyCachePath is where the last-known fleet policy is persisted.
// Written 0600 next to the rest of the ship's control state.
//
// Persisting it is what makes a reboot fail CLOSED. Before this existed, the
// evaluator's fleet slot lived only in memory: a ship that restarted while
// Fleet Command was unreachable came up with no fleet deny list at all and ran
// every mate without the Admiral's unconditional floor until a fetch finally
// succeeded — a network outage silently switching off a security control.
func fleetPolicyCachePath() string {
	return filepath.Join(project.Dir, fleetPolicyCacheName)
}

// startFleetPolicy installs the cached policy, fires an initial fetch and
// starts a background poller that keeps the ship's evaluator in sync with
// Fleet Command's fleet-policy.yaml. No-op when no fleet URL is configured (a
// ship-only deploy has no Admiral to answer to). Called from Run after
// startFleet so fleetURL/fleetToken are set.
//
// Failure policy: on a fetch error we KEEP THE LAST-KNOWN policy — the one
// just fetched in this process, or the one persisted by a previous run.
// Failing open (drop the deny list) turns a network blip into a security
// regression; failing closed in the "deny everything" sense would break every
// offline ship. Last-known, across restarts, is the reasonable middle.
func (s *Server) startFleetPolicy(ctx context.Context, conf *project.Config) {
	if conf == nil || strings.TrimSpace(conf.Fleet.URL) == "" {
		return
	}
	if s.perms == nil {
		return
	}
	// Install the cached policy BEFORE anything else: the first tool call can
	// land microseconds after boot, well before any network round trip, and
	// the cache is what has to hold the line if the fetch never succeeds.
	cached := s.loadCachedFleetPolicy()
	base := strings.TrimRight(strings.TrimSpace(conf.Fleet.URL), "/")
	if _, err := fleeturl.Validate(base); err != nil {
		// Same refusal as the tunnel: a plaintext fleet URL means a man in the
		// middle could serve an empty deny list and turn the Admiral's floor
		// off. Refusing the fetch leaves the cached policy in force rather
		// than replacing it with an attacker's.
		slog.Error("fleet-policy: refusing to fetch over an unusable fleet url", "url", conf.Fleet.URL, "err", err)
		if !cached {
			slog.Error("fleet-policy: no cached policy either — this ship is running WITHOUT the fleet-wide deny list")
		}
		return
	}
	go s.fleetPolicyLoop(ctx, base, conf.Fleet.Token(), cached)
}

// fleetPolicyLoop fetches now and then on a ticker, retrying fast while no
// policy at all is in force. have reports whether a policy (from the on-disk
// cache) is already installed.
func (s *Server) fleetPolicyLoop(ctx context.Context, baseURL, token string, have bool) {
	for {
		if err := s.fetchFleetPolicy(ctx, baseURL, token); err != nil {
			// Deliberately do NOT clear the evaluator's cached policy.
			if have {
				slog.Warn("fleet-policy: fetch failed, keeping last-known policy", "err", err)
			} else {
				slog.Error("fleet-policy: fetch failed and no cached policy is in force — "+
					"this ship is running WITHOUT the fleet-wide deny list", "err", err)
			}
		} else {
			have = true
		}

		wait := fleetPolicyPollInterval
		if !have {
			wait = fleetPolicyRetryInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-time.After(wait):
		}
	}
}

// fetchFleetPolicy pulls the current fleet deny list from Fleet Command,
// installs it on the permissions evaluator and persists it for the next boot.
// Returns an error on any I/O or decode failure; the caller keeps the
// last-known cache in that case.
func (s *Server) fetchFleetPolicy(ctx context.Context, baseURL, token string) error {
	if _, err := fleeturl.Validate(baseURL); err != nil {
		return err
	}
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
	// Bounded read: one byte over the cap is enough to tell the difference
	// between "big policy" and "endpoint streaming at us forever".
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFleetPolicyBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxFleetPolicyBytes {
		return fmt.Errorf("fleet policy exceeds %d bytes; refusing to decode", maxFleetPolicyBytes)
	}
	var pol permissions.FleetPolicy
	if err := json.Unmarshal(body, &pol); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	s.perms.SetFleetPolicy(&pol)
	if len(pol.Deny) == 0 {
		// Worth a line of its own: an empty deny list is a legitimate Admiral
		// choice, and it is also exactly what a man in the middle would serve.
		// With plaintext refused it cannot be forged silently, but the
		// operator should still be able to see when the floor went away.
		slog.Warn("fleet-policy: refreshed with an EMPTY deny list — no fleet-wide floor is in force")
	} else {
		slog.Debug("fleet-policy: refreshed", "deny_rules", len(pol.Deny))
	}
	if err := saveFleetPolicyCache(&pol); err != nil {
		// Not fatal: the policy is in force for this run either way. It only
		// costs us the fail-closed guarantee on the next boot.
		slog.Warn("fleet-policy: could not persist the policy cache", "path", fleetPolicyCachePath(), "err", err)
	}
	return nil
}

// loadCachedFleetPolicy installs the policy persisted by a previous run and
// reports whether one was found. A missing cache is normal (first boot); a
// corrupt one is logged and ignored, since refusing to boot over it would be a
// worse failure than the fetch that is about to replace it anyway.
func (s *Server) loadCachedFleetPolicy() bool {
	b, err := os.ReadFile(fleetPolicyCachePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("fleet-policy: cannot read the policy cache", "path", fleetPolicyCachePath(), "err", err)
		}
		return false
	}
	var pol permissions.FleetPolicy
	if err := json.Unmarshal(b, &pol); err != nil {
		slog.Warn("fleet-policy: ignoring a corrupt policy cache", "path", fleetPolicyCachePath(), "err", err)
		return false
	}
	if len(pol.Deny) == 0 {
		return false
	}
	s.perms.SetFleetPolicy(&pol)
	slog.Info("fleet-policy: applied the cached policy from the last run", "deny_rules", len(pol.Deny))
	return true
}

// saveFleetPolicyCache persists the policy for the next boot. The re-marshal
// is deliberate: we store what we parsed, not the bytes a remote sent, so a
// hostile endpoint cannot smuggle anything through the cache file.
func saveFleetPolicyCache(pol *permissions.FleetPolicy) error {
	if pol == nil {
		return nil
	}
	b, err := json.Marshal(pol)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(project.Dir, 0o755); err != nil {
		return err
	}
	return project.WritePrivateFile(fleetPolicyCachePath(), b)
}

// ErrNoFleetURL is returned by callers that need a fleet URL but the ship
// isn't wired to a fleet. Not used internally — fleet policy is silently a
// no-op in that case — but exposed for CLI diagnostics.
var ErrNoFleetURL = errors.New("no fleet URL configured")
