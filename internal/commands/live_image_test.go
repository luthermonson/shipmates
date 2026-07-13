package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestImageLiveCLIForwardsOrderedPathsAndZeroImageOmission(t *testing.T) {
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
	for _, args := range [][]string{{"live", "--image", "space a.png", "--image", "-dash.png", "backend", "inspect"}, {"live", "backend", "plain"}} {
		cmd := Live()
		cmd.Writer = &bytes.Buffer{}
		cmd.ErrWriter = &bytes.Buffer{}
		if e := cmd.Run(context.Background(), args); e != nil {
			t.Fatal(e)
		}
	}
	images := bodies[0]["images"].([]any)
	if images[0] != "space a.png" || images[1] != "-dash.png" {
		t.Fatalf("order=%v", images)
	}
	if _, ok := bodies[1]["images"]; ok {
		t.Fatal("zero-image request changed")
	}
	for _, b := range bodies {
		if strings.Contains(b["prompt"].(string), ".png") {
			t.Fatal("path embedded in prompt")
		}
	}
}
