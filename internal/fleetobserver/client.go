package fleetobserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

const maxResponseBytes = 2 << 20

// Client is the closed M7 observer client. Credential is never placed in a URL.
type Client struct {
	BaseURL, Credential string
	HTTP                *http.Client
}

func (c Client) get(ctx context.Context, path string, out any) error {
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !loopbackHTTP(base)) {
		return errors.New("invalid_fleet_configuration")
	}
	if c.Credential == "" || strings.ContainsAny(c.Credential, "\r\n \t") || strings.Count(c.Credential, ".") != 1 {
		return errors.New("invalid_observer_credential")
	}
	rel, _ := url.Parse(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.ResolveReference(rel).String(), nil)
	if err != nil {
		return errors.New("invalid_fleet_configuration")
	}
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	req.Header.Set("Accept", "application/json")
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_refused") }}
	}
	resp, err := h.Do(req)
	if err != nil {
		return errors.New("fleet_unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(b) > maxResponseBytes {
		return errors.New("invalid_fleet_response")
	}
	if resp.StatusCode != http.StatusOK {
		var e ErrorV1
		if json.Unmarshal(b, &e) != nil || e.Code == "" {
			return errors.New("fleet_request_failed")
		}
		return errors.New(e.Code)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid_fleet_response")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("invalid_fleet_response")
	}
	return nil
}

func loopbackHTTP(u *url.URL) bool {
	if u.Scheme != "http" || u.Host == "" {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback())
}

func (c Client) Ships(ctx context.Context) (ShipsV1, error) {
	var v ShipsV1
	e := c.get(ctx, basePath+"/ships", &v)
	return v, e
}
func (c Client) Status(ctx context.Context, id string) (ShipV1, error) {
	if !validOpaque(id) {
		return ShipV1{}, errors.New("invalid_ship_id")
	}
	var v ShipV1
	e := c.get(ctx, basePath+"/ships/"+url.PathEscape(id), &v)
	return v, e
}
func (c Client) Events(ctx context.Context, after string, limit int, stream bool) (fleetobserve.ReadResult, error) {
	if limit < 1 {
		return fleetobserve.ReadResult{}, errors.New("invalid_limit")
	}
	p := basePath + "/events"
	if stream {
		p += "/stream"
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	var v fleetobserve.ReadResult
	e := c.get(ctx, p+"?"+q.Encode(), &v)
	return v, e
}

func Cursor(epoch string, cursor uint64) string { return fmt.Sprintf("%s:%d", epoch, cursor) }

// AdvanceCursor returns the authoritative continuation cursor for a successful
// event response, including a scoped response whose visible event list is empty.
func AdvanceCursor(previous string, r fleetobserve.ReadResult) (string, error) {
	epoch := ""
	if r.Snapshot != nil {
		epoch = r.Snapshot.FleetEpoch
	} else if r.Gap != nil {
		epoch = r.Gap.CurrentEpoch
	} else if len(r.Events) > 0 {
		epoch = r.Events[0].FleetEpoch
	} else if previous != "" {
		epoch, _, _ = strings.Cut(previous, ":")
	}
	if !validOpaque(epoch) {
		return "", errors.New("invalid_fleet_response")
	}
	return Cursor(epoch, r.NextCursor), nil
}
