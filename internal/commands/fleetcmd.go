package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/fleet"
	"github.com/urfave/cli/v3"
)

// Fleet is the central-rendezvous subcommand tree:
//
//	shipmates fleet serve     — run the rendezvous server
//	shipmates fleet ls        — list connected captains
//	shipmates fleet tail      — tail a captain's event feed (one-shot)
//	shipmates fleet tell      — inject a message to a crew persona via a captain
//	shipmates fleet pending   — list pending permission prompts on a captain
//	shipmates fleet resolve   — allow/deny a pending permission prompt
//
// The operator commands talk to the *fleet*'s local HTTP API (env
// SHIPMATES_FLEET_URL, default http://127.0.0.1:8443). The fleet then proxies
// through its remotedialer tunnel back to the named captain's localhost server.
//
// Auth: the shared secret is read from $SHIPMATES_FLEET_TOKEN, or via
// --token-file <path>, and sent as Authorization: Bearer on every request. No
// --token flag (cmdline visibility). Empty token = no auth (matches a fleet
// started without a token, dev only).
func Fleet() *cli.Command {
	return &cli.Command{
		Name:  "fleet",
		Usage: "central rendezvous for many shipmates leads",
		Commands: []*cli.Command{
			fleetServe(),
			fleetLs(),
			fleetTail(),
			fleetTell(),
			fleetShow(),
			fleetPending(),
			fleetResolve(),
			fleetStatus(),
			fleetBeads(),
			fleetDispatch(),
		},
	}
}

// operatorFlags are the shared --fleet URL + --token-file flags every
// operator command needs. Defined once so we don't drift across subcommands.
func operatorFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "fleet", Sources: cli.EnvVars("SHIPMATES_FLEET_URL"), Value: "http://127.0.0.1:8443", Usage: "fleet URL"},
		&cli.StringFlag{Name: "token-file", Usage: "read the fleet token from this file (overrides $SHIPMATES_FLEET_TOKEN)"},
	}
}

func fleetServe() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "run the fleet HTTP/websocket server",
		Description: "Shared secret is read from $SHIPMATES_FLEET_TOKEN, or from the\n" +
			"file named by --token-file. The secret is never accepted as a CLI flag —\n" +
			"that would put it in ps/cmdline. Empty token disables auth (dev only).",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "addr", Value: "127.0.0.1:8443", Usage: "listen address"},
			&cli.StringFlag{Name: "token-file", Usage: "read the shared secret from this file (preferred over env on shared hosts)"},
			&cli.StringFlag{Name: "store", Usage: "optional SQLite path to mirror events; absent = ephemeral live-only"},
			&cli.StringFlag{Name: "llm-backend", Sources: cli.EnvVars("LLM_BACKEND"), Value: "openai", Usage: "conversation brain: 'openai' (chat-completions at --llm-url) or 'claude-cli' (spawn claude -p on this host, subscription auth)"},
			&cli.StringFlag{Name: "llm-key-env", Value: "LLM_API_KEY", Usage: "env var holding the bearer key for hosted OAI endpoints (Anthropic/OpenAI/Groq); unset var = no auth"},
			&cli.StringFlag{Name: "llm-url", Sources: cli.EnvVars("LLM_URL"), Usage: "OpenAI-compatible base URL incl. /v1 (ollama: http://127.0.0.1:11434/v1); enables /api/conversation"},
			&cli.StringFlag{Name: "llm-model", Sources: cli.EnvVars("LLM_MODEL"), Value: "qwen2.5:7b", Usage: "model name for the conversation loop (7b-class is the CPU sweet spot; bigger wants a GPU)"},
			&cli.StringFlag{Name: "tts-voice", Sources: cli.EnvVars("TTS_VOICE"), Value: "en-US-AriaNeural", Usage: "Edge TTS voice for /api/tts (e.g. en-US-JennyNeural). Empty disables."},
			&cli.StringFlag{Name: "tts-url", Sources: cli.EnvVars("TTS_URL"), Usage: "OpenAI-compatible /v1/audio/speech endpoint (kokoro-fastapi, speaches, ...); overrides Edge TTS"},
			&cli.StringFlag{Name: "tts-model", Sources: cli.EnvVars("TTS_MODEL"), Usage: "model field for OAI-style TTS servers"},
			&cli.StringFlag{Name: "stt-url", Sources: cli.EnvVars("STT_URL"), Usage: "enable /api/stt: whisper.cpp /inference or an OpenAI-compatible /v1/audio/transcriptions endpoint"},
			&cli.StringFlag{Name: "stt-model", Sources: cli.EnvVars("STT_MODEL"), Usage: "model field for OAI-style STT servers (whisper.cpp ignores it)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			token, err := loadFleetToken(c.String("token-file"))
			if err != nil {
				return err
			}
			b, err := fleet.New(fleet.Options{
				Addr:        c.String("addr"),
				Token:       token,
				Store:       c.String("store"),
				LLMBackend:  c.String("llm-backend"),
				LLMURL:      c.String("llm-url"),
				LLMModel:    c.String("llm-model"),
				LLMKey:      strings.TrimSpace(os.Getenv(c.String("llm-key-env"))),
				TTSVoice:    c.String("tts-voice"),
				TTSURL:      c.String("tts-url"),
				TTSModel:    c.String("tts-model"),
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

// loadFleetToken resolves the fleet's shared secret with this precedence:
// --token-file (if set) > $SHIPMATES_FLEET_TOKEN > empty (no auth). Reading
// from a file is preferred on shared hosts where same-uid processes can read
// /proc/<pid>/environ.
func loadFleetToken(file string) (string, error) {
	if file = strings.TrimSpace(file); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv("SHIPMATES_FLEET_TOKEN")), nil
}

func fleetLs() *cli.Command {
	return &cli.Command{
		Name:  "ls",
		Usage: "list captains known to the fleet",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			body, err := fleetGet(ctx, c,"/api/captains")
			if err != nil {
				return err
			}
			var captains []struct {
				ClientKey string    `json:"client_key"`
				Repo      string    `json:"repo"`
				Persona   string    `json:"persona"`
				Port      int       `json:"port"`
				FirstSeen time.Time `json:"first_seen"`
				LastSeen  time.Time `json:"last_seen"`
				Connected bool      `json:"connected"`
			}
			if err := json.Unmarshal(body, &captains); err != nil {
				return fmt.Errorf("decode captains: %w", err)
			}
			if len(captains) == 0 {
				fmt.Println("(no captains connected)")
				return nil
			}
			for _, l := range captains {
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

func fleetTail() *cli.Command {
	return &cli.Command{
		Name:      "tail",
		Usage:     "tail a captain's event feed (one-shot snapshot of the live feed)",
		ArgsUsage: "<captain-key>",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			key := c.Args().First()
			if key == "" {
				return errors.New("usage: shipmates fleet tail <captain-key>")
			}
			body, err := fleetGet(ctx, c,"/api/captain/"+key+"/feed")
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(body)
			return nil
		},
	}
}

func fleetTell() *cli.Command {
	return &cli.Command{
		Name:      "tell",
		Usage:     "inject a message to a persona on a connected captain",
		ArgsUsage: "<captain-key> <persona> <message>",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 3 {
				return errors.New("usage: shipmates fleet tell <captain-key> <persona> <message>")
			}
			key, persona := args[0], args[1]
			msg := strings.Join(args[2:], " ")
			payload, _ := json.Marshal(map[string]string{"message": msg})
			_, err := fleetPost(ctx, c,"/api/captain/"+key+"/tell/"+persona, payload)
			return err
		},
	}
}

// fleetShow uploads a local file to a captain via Fleet Command's attach
// endpoint. The bytes land in the target ship's .shipmates/inbox/ and the ship
// auto-tells the mate to look at the file — the mate reads it via Claude
// Code's own multi-modal Read tool. Shipmates does not talk to Anthropic
// directly; the file is just transport.
func fleetShow() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "attach a file (photo, PDF, screenshot, text) to a captain — the mate reads it via Claude Code's Read tool",
		ArgsUsage: "<captain-key> <file-path>",
		Flags: append(operatorFlags(),
			&cli.StringFlag{Name: "caption", Usage: "optional caption sent alongside the file"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 2 {
				return errors.New("usage: shipmates fleet show <captain-key> <file-path>")
			}
			key, filePath := args[0], args[1]
			caption := c.String("caption")

			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("open %s: %w", filePath, err)
			}
			defer f.Close()

			var body bytes.Buffer
			w := multipart.NewWriter(&body)
			fw, err := w.CreateFormFile("file", filepath.Base(filePath))
			if err != nil {
				return fmt.Errorf("create form file: %w", err)
			}
			if _, err := io.Copy(fw, f); err != nil {
				return fmt.Errorf("copy file: %w", err)
			}
			if caption != "" {
				if err := w.WriteField("caption", caption); err != nil {
					return fmt.Errorf("write caption: %w", err)
				}
			}
			if err := w.Close(); err != nil {
				return fmt.Errorf("close multipart: %w", err)
			}

			out, err := fleetDoRaw(ctx, c, "POST", "/api/captain/"+key+"/attach", w.FormDataContentType(), body.Bytes())
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(out)
			fmt.Println()
			return nil
		},
	}
}

func fleetPending() *cli.Command {
	return &cli.Command{
		Name:      "pending",
		Usage:     "list pending permission prompts on a captain",
		ArgsUsage: "<captain-key>",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			key := c.Args().First()
			if key == "" {
				return errors.New("usage: shipmates fleet pending <captain-key>")
			}
			body, err := fleetGet(ctx, c,"/api/captain/"+key+"/pending")
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(body)
			return nil
		},
	}
}

func fleetResolve() *cli.Command {
	return &cli.Command{
		Name:      "resolve",
		Usage:     "allow|deny a pending permission prompt on a captain",
		ArgsUsage: "<captain-key> <id> allow|deny",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 3 {
				return errors.New("usage: shipmates fleet resolve <captain-key> <id> allow|deny")
			}
			key, id, behavior := args[0], args[1], args[2]
			if behavior != "allow" && behavior != "deny" {
				return fmt.Errorf("behavior must be allow|deny, got %q", behavior)
			}
			payload, _ := json.Marshal(map[string]string{"behavior": behavior})
			_, err := fleetPost(ctx, c,"/api/captain/"+key+"/resolve/"+id, payload)
			return err
		},
	}
}

func fleetStatus() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "per-mate status dots across every connected ship",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			body, err := fleetGet(ctx, c, "/api/status")
			if err != nil {
				return err
			}
			var mates []struct {
				ClientKey string `json:"client_key"`
				Persona   string `json:"persona"`
				Status    string `json:"status"`
			}
			if err := json.Unmarshal(body, &mates); err != nil {
				return fmt.Errorf("decode status: %w", err)
			}
			for _, m := range mates {
				fmt.Printf("%-9s %s/%s\n", m.Status, m.ClientKey, m.Persona)
			}
			return nil
		},
	}
}

func fleetBeads() *cli.Command {
	return &cli.Command{
		Name:      "beads",
		Usage:     "list open beads (fleet-wide, or one ship's graph)",
		ArgsUsage: "[captain-key]",
		Flags:     operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "/api/beads"
			if key := c.Args().First(); key != "" {
				path = "/api/captain/" + key + "/beads"
			}
			body, err := fleetGet(ctx, c, path)
			if err != nil {
				return err
			}
			var beads []struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Status   string `json:"status"`
				Priority *int   `json:"priority"`
				Assignee string `json:"assignee"`
			}
			if err := json.Unmarshal(body, &beads); err != nil {
				return fmt.Errorf("decode beads: %w", err)
			}
			if len(beads) == 0 {
				fmt.Println("(no open beads)")
				return nil
			}
			for _, b := range beads {
				prio := ""
				if b.Priority != nil {
					prio = fmt.Sprintf("p%d", *b.Priority)
				}
				assignee := b.Assignee
				if assignee != "" {
					assignee = " → " + assignee
				}
				fmt.Printf("%-12s %-3s %-8s %s%s\n", b.ID, prio, b.Status, b.Title, assignee)
			}
			return nil
		},
	}
}

func fleetDispatch() *cli.Command {
	return &cli.Command{
		Name:      "dispatch",
		Usage:     "assign a bead to persona@ship and wake the mate to work it",
		ArgsUsage: "<carrying-captain-key> <bead-id> <target-captain-key> <persona>",
		Flags:     operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			args := c.Args().Slice()
			if len(args) < 4 {
				return errors.New("usage: shipmates fleet dispatch <carrying-captain-key> <bead-id> <target-captain-key> <persona>")
			}
			payload, _ := json.Marshal(map[string]string{"ship": args[2], "persona": args[3]})
			out, err := fleetPost(ctx, c, "/api/captain/"+args[0]+"/bead/"+args[1]+"/assign", payload)
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(out)
			fmt.Println()
			return nil
		},
	}
}

// fleetGet/fleetPost issue requests to the fleet's local HTTP API,
// authenticated via Authorization: Bearer (token resolved from --token-file
// or $SHIPMATES_FLEET_TOKEN). 401 surfaces as a clean "unauthorized" error
// so the operator knows it's an auth problem, not a network issue.
func fleetGet(ctx context.Context, c *cli.Command, path string) ([]byte, error) {
	return fleetDo(ctx, c, "GET", path, nil)
}

func fleetPost(ctx context.Context, c *cli.Command, path string, body []byte) ([]byte, error) {
	return fleetDo(ctx, c, "POST", path, body)
}

// fleetDoRaw is the multipart / arbitrary-Content-Type sibling of fleetDo.
// Used by `fleet show` to POST multipart/form-data uploads. The bearer
// token, base URL resolution, and error handling all match fleetDo.
func fleetDoRaw(ctx context.Context, c *cli.Command, method, path, contentType string, body []byte) ([]byte, error) {
	base := strings.TrimRight(c.String("fleet"), "/")
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	token, err := loadFleetToken(c.String("token-file"))
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
		return out, fmt.Errorf("unauthorized — set $SHIPMATES_FLEET_TOKEN or pass --token-file")
	}
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("fleet %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func fleetDo(ctx context.Context, c *cli.Command, method, path string, body []byte) ([]byte, error) {
	base := strings.TrimRight(c.String("fleet"), "/")
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
	token, err := loadFleetToken(c.String("token-file"))
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
		return out, fmt.Errorf("unauthorized — set $SHIPMATES_FLEET_TOKEN or pass --token-file")
	}
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("fleet %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}
