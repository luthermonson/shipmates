package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestLiveNeverForwardsImages locks in the removal of `--image` from live:
// the start request carries prompt and fresh only, and no path is smuggled
// into the prompt. Attachments go through `shipmates show`.
func TestLiveNeverForwardsImages(t *testing.T) {
	t.Chdir(t.TempDir())
	writeLiveCommandDiscovery(t)
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	var bodies []map[string]any
	http.DefaultTransport = liveRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(200, "ok", map[string]string{"X-Shipmates-Project": r.Header.Get("X-Shipmates-Project")}), nil
		}
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		bodies = append(bodies, v)
		return response(200, `{"persona":"backend","session_id":"s","thread_id":"t","turn_id":"u","state":"working"}`, nil), nil
	})
	cmd := Live()
	cmd.Writer = &bytes.Buffer{}
	cmd.ErrWriter = &bytes.Buffer{}
	if e := cmd.Run(context.Background(), []string{"live", "backend", "plain"}); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	if _, ok := bodies[0]["images"]; ok {
		t.Fatalf("live still forwards images: %v", bodies[0])
	}
	if bodies[0]["prompt"] != "plain" {
		t.Fatalf("prompt = %v", bodies[0]["prompt"])
	}
}
