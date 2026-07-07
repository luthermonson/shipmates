package fleet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// PTY proxying. The buffered b.proxy() helper is fine for start/input/resize
// (small request, small response), but the stream endpoint is a long-lived SSE
// byte flow — it needs a real streaming path: dial the captain through the
// remotedialer tunnel, issue the GET, and pump the response body to the
// browser flush-per-chunk.

// captainTransport returns an http.RoundTripper that dials the given
// captain's local server through its tunnel, regardless of the request URL's
// host.
func (b *Server) captainTransport(clientKey string) (http.RoundTripper, error) {
	b.mu.Lock()
	captain, ok := b.captains[clientKey]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such captain: %s", clientKey)
	}
	if !b.dialer.HasSession(clientKey) {
		return nil, fmt.Errorf("captain %s not currently connected", clientKey)
	}
	dial := b.dialer.Dialer(clientKey)
	addr := fmt.Sprintf("127.0.0.1:%d", captain.Port)
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, "tcp", addr)
		},
		// One conn per stream; don't pool tunneled conns across captains.
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 15 * time.Second,
	}, nil
}

// proxyPTYPost forwards a small POST (start/input/resize/takeover/release)
// to the captain. The query string rides along — it carries the viewer's
// writer-lock client id.
func (b *Server) proxyPTYPost(captainPathFmt string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		persona := r.PathValue("persona")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		path := fmt.Sprintf(captainPathFmt, persona)
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		out, status, err := b.proxy(r.Context(), key, "POST", path, body)
		writeProxied(w, status, out, err)
	}
}

// proxyPost forwards a small POST to a fixed path on the captain.
func (b *Server) proxyPost(captainPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		out, status, err := b.proxy(r.Context(), key, "POST", captainPath, body)
		writeProxied(w, status, out, err)
	}
}

// proxyPost2 is proxyPost with one extra path parameter interpolated.
func (b *Server) proxyPost2(captainPathFmt, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		val := r.PathValue(param)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		out, status, err := b.proxy(r.Context(), key, "POST", fmt.Sprintf(captainPathFmt, val), body)
		writeProxied(w, status, out, err)
	}
}

// proxyGet2 is proxyGet with one extra path parameter interpolated.
func (b *Server) proxyGet2(captainPathFmt, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		val := r.PathValue(param)
		out, status, err := b.proxy(r.Context(), key, "GET", fmt.Sprintf(captainPathFmt, val), nil)
		writeProxied(w, status, out, err)
	}
}

// handlePTYStreamProxy pipes the captain's /pty/{persona}/stream SSE response
// through to the browser. Chunks are flushed as they arrive so xterm.js sees
// keystroke-latency updates, not 4KB buffers.
func (b *Server) handlePTYStreamProxy(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	persona := r.PathValue("persona")

	rt, err := b.captainTransport(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET",
		fmt.Sprintf("http://captain/pty/%s/stream", persona), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		http.Error(w, "dial captain stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		http.Error(w, string(msg), resp.StatusCode)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			fl.Flush()
		}
		if err != nil {
			return
		}
	}
}
