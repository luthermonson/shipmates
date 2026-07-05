package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/bridge"
	"github.com/urfave/cli/v3"
)

// Bridge is the central-rendezvous subcommand tree:
//
//	shipmates bridge serve     — run the rendezvous server
//	shipmates bridge ls        — list connected leads
//	shipmates bridge tail      — tail a lead's event feed (one-shot)
//	shipmates bridge tell      — inject a message to a crew persona via a lead
//	shipmates bridge pending   — list pending permission prompts on a lead
//	shipmates bridge resolve   — allow/deny a pending permission prompt
//
// The operator commands talk to the *bridge*'s local HTTP API (env
// SHIPMATES_BRIDGE_URL, default http://127.0.0.1:8443). The bridge then proxies
// through its remotedialer tunnel back to the named lead's localhost server.
//
// Auth: the shared secret is read from $SHIPMATES_BRIDGE_TOKEN, or via
// --token-file <path>, and sent as Authorization: Bearer on every request. No
// --token flag (cmdline visibility). Empty token = no auth (matches a bridge
// started without a token, dev only).
func Bridge() *cli.Command {
	return &cli.Command{
		Name:  "bridge",
		Usage: "central rendezvous for many shipmates leads",
		Commands: []*cli.Command{
			bridgeServe(),
			bridgeLs(),
			bridgeTail(),
			bridgeTell(),
			bridgePending(),
			bridgeResolve(),
		},
	}
}

// operatorFlags are the shared --bridge URL + --token-file flags every
// operator command needs. Defined once so we don't drift across subcommands.
func operatorFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "bridge", Sources: cli.EnvVars("SHIPMATES_BRIDGE_URL"), Value: "http://127.0.0.1:8443", Usage: "bridge URL"},
		&cli.StringFlag{Name: "token-file", Usage: "read the bridge token from this file (overrides $SHIPMATES_BRIDGE_TOKEN)"},
	}
}

func bridgeServe() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "run the bridge HTTP/websocket server",
		Description: "Shared secret is read from $SHIPMATES_BRIDGE_TOKEN, or from the\n" +
			"file named by --token-file. The secret is never accepted as a CLI flag —\n" +
			"that would put it in ps/cmdline. Empty token disables auth (dev only).",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "addr", Value: "127.0.0.1:8443", Usage: "listen address"},
			&cli.StringFlag{Name: "token-file", Usage: "read the shared secret from this file (preferred over env on shared hosts)"},
			&cli.StringFlag{Name: "store", Usage: "optional SQLite path to mirror events; absent = ephemeral live-only"},
			&cli.StringFlag{Name: "ollama-url", Sources: cli.EnvVars("OLLAMA_URL"), Usage: "enable /api/conversation by pointing at a local Ollama (e.g. http://127.0.0.1:11434)"},
			&cli.StringFlag{Name: "ollama-model", Sources: cli.EnvVars("OLLAMA_MODEL"), Value: "qwen2.5:7b", Usage: "Ollama model tag for the conversation loop"},
			&cli.BoolFlag{Name: "ollama-cpu", Sources: cli.EnvVars("OLLAMA_CPU"), Usage: "force CPU inference (num_gpu=0) — for hosts whose GPU ollama can't actually run"},
			&cli.StringFlag{Name: "tts-voice", Sources: cli.EnvVars("TTS_VOICE"), Value: "en-US-AriaNeural", Usage: "Edge TTS voice for /api/tts (e.g. en-US-JennyNeural). Empty disables."},
			&cli.StringFlag{Name: "stt-url", Sources: cli.EnvVars("STT_URL"), Usage: "enable /api/stt: whisper.cpp /inference or an OpenAI-compatible /v1/audio/transcriptions endpoint"},
			&cli.StringFlag{Name: "stt-model", Sources: cli.EnvVars("STT_MODEL"), Usage: "model field for OAI-style STT servers (whisper.cpp ignores it)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			token, err := loadBridgeToken(c.String("token-file"))
			if err != nil {
				return err
			}
			b, err := bridge.New(bridge.Options{
				Addr:        c.String("addr"),
				Token:       token,
				Store:       c.String("store"),
				OllamaURL:   c.String("ollama-url"),
				OllamaModel: c.String("ollama-model"),
				OllamaCPU:   c.Bool("ollama-cpu"),
				TTSVoice:    c.String("tts-voice"),
				STTURL:      c.String("stt-url"),
				STTModel:    c.String("stt-model"),
			})
			if err != nil {
				return err
			}
			defer b.Close()
			return b.Run(ctx, c.String("addr"))
		},
	}
}

// loadBridgeToken resolves the bridge's shared secret with this precedence:
// --token-file (if set) > $SHIPMATES_BRIDGE_TOKEN > empty (no auth). Reading
// from a file is preferred on shared hosts where same-uid processes can read
// /proc/<pid>/environ.
func loadBridgeToken(file string) (string, error) {
	if file = strings.TrimSpace(file); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv("SHIPMATES_BRIDGE_TOKEN")), nil
}

func bridgeLs() *cli.Command {
	return &cli.Command{
		Name:  "ls",
		Usage: "list leads known to the bridge",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			body, err := bridgeGet(ctx, c,"/api/leads")
			if err != nil {
				return err
			}
			var leads []struct {
				ClientKey string    `json:"client_key"`
				Repo      string    `json:"repo"`
				Persona   string    `json:"persona"`
				Port      int       `json:"port"`
				FirstSeen time.Time `json:"first_seen"`
				LastSeen  time.Time `json:"last_seen"`
				Connected bool      `json:"connected"`
			}
			if err := json.Unmarshal(body, &leads); err != nil {
				return fmt.Errorf("decode leads: %w", err)
			}
			if len(leads) == 0 {
				fmt.Println("(no leads connected)")
				return nil
			}
			for _, l := range leads {
				state := "offline"
				if l.Connected {
					state = "online"
				}
				fmt.Printf("%-9s %s  (port %d, last %s)\n", state, l.ClientKey, l.Port, l.LastSeen.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func bridgeTail() *cli.Command {
	return &cli.Command{
		Name:      "tail",
		Usage:     "tail a lead's event feed (one-shot snapshot of the live feed)",
		ArgsUsage: "<lead-key>",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			key := c.Args().First()
			if key == "" {
				return errors.New("usage: shipmates bridge tail <lead-key>")
			}
			body, err := bridgeGet(ctx, c,"/api/lead/"+key+"/feed")
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(body)
			return nil
		},
	}
}

func bridgeTell() *cli.Command {
	return &cli.Command{
		Name:      "tell",
		Usage:     "inject a message to a persona on a connected lead",
		ArgsUsage: "<lead-key> <persona> <message>",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 3 {
				return errors.New("usage: shipmates bridge tell <lead-key> <persona> <message>")
			}
			key, persona := args[0], args[1]
			msg := strings.Join(args[2:], " ")
			payload, _ := json.Marshal(map[string]string{"message": msg})
			_, err := bridgePost(ctx, c,"/api/lead/"+key+"/tell/"+persona, payload)
			return err
		},
	}
}

func bridgePending() *cli.Command {
	return &cli.Command{
		Name:      "pending",
		Usage:     "list pending permission prompts on a lead",
		ArgsUsage: "<lead-key>",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			key := c.Args().First()
			if key == "" {
				return errors.New("usage: shipmates bridge pending <lead-key>")
			}
			body, err := bridgeGet(ctx, c,"/api/lead/"+key+"/pending")
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(body)
			return nil
		},
	}
}

func bridgeResolve() *cli.Command {
	return &cli.Command{
		Name:      "resolve",
		Usage:     "allow|deny a pending permission prompt on a lead",
		ArgsUsage: "<lead-key> <id> allow|deny",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 3 {
				return errors.New("usage: shipmates bridge resolve <lead-key> <id> allow|deny")
			}
			key, id, behavior := args[0], args[1], args[2]
			if behavior != "allow" && behavior != "deny" {
				return fmt.Errorf("behavior must be allow|deny, got %q", behavior)
			}
			payload, _ := json.Marshal(map[string]string{"behavior": behavior})
			_, err := bridgePost(ctx, c,"/api/lead/"+key+"/resolve/"+id, payload)
			return err
		},
	}
}

// bridgeGet/bridgePost issue requests to the bridge's local HTTP API,
// authenticated via Authorization: Bearer (token resolved from --token-file
// or $SHIPMATES_BRIDGE_TOKEN). 401 surfaces as a clean "unauthorized" error
// so the operator knows it's an auth problem, not a network issue.
func bridgeGet(ctx context.Context, c *cli.Command, path string) ([]byte, error) {
	return bridgeDo(ctx, c, "GET", path, nil)
}

func bridgePost(ctx context.Context, c *cli.Command, path string, body []byte) ([]byte, error) {
	return bridgeDo(ctx, c, "POST", path, body)
}

func bridgeDo(ctx context.Context, c *cli.Command, method, path string, body []byte) ([]byte, error) {
	base := strings.TrimRight(c.String("bridge"), "/")
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token, err := loadBridgeToken(c.String("token-file"))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return out, fmt.Errorf("unauthorized — set $SHIPMATES_BRIDGE_TOKEN or pass --token-file")
	}
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("bridge %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}
