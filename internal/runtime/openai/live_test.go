package openai

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// ollamaBaseURL is the conventional local Ollama OpenAI-compatible endpoint.
// Nothing is installed or started by this test: it checks whether one happens
// to be listening and skips when it is not, so `go test ./...` behaves the same
// on a machine with no inference server.
const ollamaBaseURL = "http://127.0.0.1:11434/v1"

// TestLiveLocalEndpoint drives the whole runtime path against a real
// OpenAI-compatible server when one is reachable at ollamaBaseURL. It is the
// only test here that talks to something we did not write, which makes it the
// only test that can catch an httptest fixture that agrees with our own bugs.
//
// It skips — never fails — when there is no endpoint, when the endpoint has no
// models loaded, or in -short mode.
func TestLiveLocalEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live endpoint test in -short mode")
	}

	probeCfg := testConfig(ollamaBaseURL)
	probeCfg.Model = "unused-for-probe"
	probeCfg.Timeout = 5 * time.Second
	probe, err := New(probeCfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		ctx, cancel := contextWithTimeout(5 * time.Second)
		defer cancel()
		_ = probe.Close(ctx)
	}()

	ctx, cancel := contextWithTimeout(5 * time.Second)
	defer cancel()
	models, err := probe.Probe(ctx)
	if err != nil {
		t.Skipf("no OpenAI-compatible endpoint reachable at %s (%v)", ollamaBaseURL, err)
	}
	if len(models) == 0 {
		t.Skipf("endpoint at %s answered /models but has no models loaded; nothing to ask", ollamaBaseURL)
	}
	model := models[0]
	t.Logf("live endpoint %s serving %d model(s); using %q", ollamaBaseURL, len(models), model)

	cfg := testConfig(ollamaBaseURL)
	cfg.Model = model
	cfg.Timeout = 90 * time.Second
	cfg.MaxTokens = 64 // keep a real GPU/CPU model from monologuing
	r := newTestRuntime(t, cfg)

	sess, err := r.StartSession(context.Background(), runtime.SessionSpec{
		Persona: "captain", ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, err := r.SendTurn(context.Background(), sess.ID(), runtime.TurnInput{
		Text: "Reply with exactly the word: ready",
	}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	got := collectTurn(t, r.Events(), 120*time.Second)
	if len(got.Errors) != 0 {
		t.Fatalf("live turn errored: %+v", got.Errors)
	}
	if strings.TrimSpace(got.Done.Text) == "" {
		t.Fatalf("live turn produced no text: %+v", got.Done)
	}
	t.Logf("live turn: %d delta(s), finish=%q usage=%+v duration=%s text=%q",
		len(got.Texts), got.Done.FinishReason, got.Done.Usage, got.Done.Duration, got.Done.Text)
	if len(got.Texts) < 1 {
		t.Errorf("expected at least one streamed delta")
	}

	// Interrupt a real generation: ask for something long, stop after the first
	// delta, and confirm the turn ends as interrupted with partial text. An
	// httptest server can be made to agree with a broken interrupt; a real one
	// cannot.
	t.Run("interrupt", func(t *testing.T) {
		turn, err := r.SendTurn(context.Background(), sess.ID(), runtime.TurnInput{
			Text: "Count from 1 to 400, one number per line, no commentary.",
		})
		if err != nil {
			t.Fatalf("SendTurn: %v", err)
		}
		var first string
		select {
		case ev := <-r.Events():
			td, ok := ev.Payload.(TextDelta)
			if !ok {
				// A reasoning delta or notice first is acceptable; only text
				// tells us generation is under way.
				t.Skipf("first live event was %v (%T), not a text delta", ev.Kind, ev.Payload)
			}
			first = td.Text
		case <-time.After(120 * time.Second):
			t.Fatal("no delta from the live endpoint")
		}
		if err := r.InterruptTurn(context.Background(), sess.ID(), turn.ID()); err != nil {
			t.Fatalf("InterruptTurn: %v", err)
		}
		done := collectTurn(t, r.Events(), 60*time.Second)
		if !done.Done.Interrupted {
			t.Errorf("live TurnDone.Interrupted = false: %+v", done.Done)
		}
		if len(done.Errors) != 0 {
			t.Errorf("interrupting a live turn produced errors: %+v", done.Errors)
		}
		t.Logf("live interrupt: first delta %q, kept %d bytes, finish=%q",
			first, len(done.Done.Text), done.Done.FinishReason)
	})
}
