package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/turninput"
	"github.com/urfave/cli/v3"
)

// Ask runs a one-shot, turn-based delegation: create the persona's session the
// first time, resume it afterward. Output streams to the terminal.
func Ask() *cli.Command {
	return &cli.Command{
		Name:      "ask",
		Usage:     "one-shot delegation to a persona (turn-based; resumes its session)",
		ArgsUsage: "<persona> <prompt>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session instead of resuming (applies config changes like model/effort)"},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Minute, Usage: "maximum wall-clock duration for the crew turn"},
			&cli.StringSliceFlag{Name: "image", Usage: "attach an existing in-project PNG, JPEG, GIF, or WebP (repeatable)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
			if persona == "" || prompt == "" {
				return errors.New("usage: shipmates ask <persona> <prompt>")
			}
			turnCtx, cancel := context.WithTimeout(ctx, c.Duration("timeout"))
			defer cancel()
			return dispatchImages(turnCtx, persona, prompt, c.Bool("fresh"), c.StringSlice("image"))
		},
	}
}

func Live() *cli.Command {
	return &cli.Command{Name: "live", Usage: "start a Codex-native live turn", ArgsUsage: "<persona> <prompt>", Flags: []cli.Flag{&cli.BoolFlag{Name: "fresh"}, &cli.StringSliceFlag{Name: "image", Usage: "attach an existing in-project raster image (repeatable)"}}, Action: func(ctx context.Context, c *cli.Command) error {
		persona := c.Args().First()
		prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
		if persona == "" || prompt == "" {
			return errors.New("usage: shipmates live <persona> <prompt>")
		}
		if err := project.ValidatePersonaName(persona); err != nil {
			return err
		}
		if err := client.EnsureRunning(); err != nil {
			return err
		}
		body := map[string]any{"prompt": prompt, "fresh": c.Bool("fresh")}
		if images := c.StringSlice("image"); len(images) > 0 {
			body["images"] = images
		}
		resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona), body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return safeServerError(resp)
		}
		var snap livesession.Snapshot
		if json.NewDecoder(resp.Body).Decode(&snap) != nil {
			return errors.New("invalid live server response")
		}
		if err := json.NewEncoder(c.Writer).Encode(snap); err != nil {
			return err
		}
		fmt.Fprintf(c.ErrWriter, "live session ready; watch with: shipmates feed %s --follow\n", persona)
		return nil
	}}
}

func safeServerError(resp *http.Response) error {
	var v struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
	if v.Code == "" {
		v.Code = "server_error"
	}
	return errors.New(v.Code)
}

// dispatch runs a one-shot turn-based delegation: resolve the persona's config,
// create the session the first time / resume it after (auto-fresh on config
// drift or when fresh is requested), apply launch flags, run, and record the
// session. Shared by `ask` and `drain`. Output streams to the terminal.
func dispatch(ctx context.Context, persona, prompt string, fresh bool) error {
	return dispatchImages(ctx, persona, prompt, fresh, nil)
}

func dispatchImages(ctx context.Context, persona, prompt string, fresh bool, paths []string) error {
	return dispatchToImages(ctx, persona, prompt, fresh, paths, os.Stdout, os.Stderr)
}

// dispatchTo is dispatch with caller-supplied output writers, so parallel
// callers (drain-many) can capture each persona's output into its own buffer.
func dispatchTo(ctx context.Context, persona, prompt string, fresh bool, stdout, stderr io.Writer) error {
	return dispatchToImages(ctx, persona, prompt, fresh, nil, stdout, stderr)
}
func dispatchToImages(ctx context.Context, persona, prompt string, fresh bool, paths []string, stdout, stderr io.Writer) error {
	installed, err := project.CanonicalPersonaAt(".", persona)
	if err != nil {
		return err
	}
	cfg, err := project.ResolvePersonaConfig(persona)
	if err != nil {
		return err
	}
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return errors.New("image_root_invalid")
	}
	batch, err := turninput.ValidateImages(root, paths)
	if err != nil {
		return err
	}
	defer batch.Close()
	return dispatchToInstalledImages(ctx, installed, prompt, fresh, cfg, batch, stdout, stderr)
}

func dispatchToInstalled(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, stdout, stderr io.Writer) error {
	persona := installed.Name
	cfg, err := project.ResolvePersonaConfig(persona)
	if err != nil {
		return err
	}
	return dispatchToInstalledImages(ctx, installed, prompt, fresh, cfg, nil, stdout, stderr)
}
func dispatchToInstalledImages(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, batch *turninput.ImageBatchV1, stdout, stderr io.Writer) error {
	return dispatchCodexInstalledImages(ctx, installed, prompt, fresh, cfg, batch, stdout, stderr)
}

// Tell sends a plain-string message to a live crew process via the server. The
// CLI translates the string to a stream-json user message server-side; the captain
// never touches JSON.
func Tell() *cli.Command {
	return &cli.Command{
		Name:      "tell",
		Usage:     "send a message to a live crew process while it works",
		ArgsUsage: "<persona> <session-id> <thread-id> <turn-id> <message>",
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			tail := c.Args().Tail()
			if persona == "" || len(tail) < 4 {
				return errors.New("usage: shipmates tell <persona> <session-id> <thread-id> <turn-id> <message>")
			}
			msg := strings.TrimSpace(strings.Join(tail[3:], " "))
			if msg == "" {
				return errors.New("usage: shipmates tell <persona> <session-id> <thread-id> <turn-id> <message>")
			}
			if err := client.EnsureRunning(); err != nil {
				return err
			}
			resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/tell", map[string]string{"session_id": tail[0], "thread_id": tail[1], "turn_id": tail[2], "message": msg})
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				return safeServerError(resp)
			}
			_, err = io.Copy(c.Writer, io.LimitReader(resp.Body, 64<<10))
			return err
		},
	}
}

// Feed prints the server's activity feed (crew output, tells, events).
func Feed() *cli.Command {
	return &cli.Command{
		Name: "feed", ArgsUsage: "<persona>", Flags: []cli.Flag{&cli.BoolFlag{Name: "follow"}, &cli.Uint64Flag{Name: "after"}},
		Usage: "print the live activity feed from the server",
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			if persona == "" {
				return errors.New("usage: shipmates feed <persona> [--follow] [--after sequence]")
			}
			if !client.Healthy() {
				return errors.New("no server running")
			}
			path := fmt.Sprintf("/api/live/%s/feed?after=%d&follow=%t", url.PathEscape(persona), c.Uint64("after"), c.Bool("follow"))
			resp, err := client.Do(ctx, http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				return safeServerError(resp)
			}
			if resp.Header.Get("X-Shipmates-History-Dropped") == "true" {
				fmt.Fprintln(c.ErrWriter, "warning: requested feed history was dropped")
			}
			_, err = io.Copy(c.Writer, resp.Body)
			return err
		},
	}
}

func Interrupt() *cli.Command {
	return &cli.Command{Name: "interrupt", Usage: "interrupt an exact Codex live turn", ArgsUsage: "<persona> <session-id> <thread-id> <turn-id>", Action: func(ctx context.Context, c *cli.Command) error {
		persona := c.Args().First()
		tail := c.Args().Tail()
		if persona == "" || len(tail) != 3 || tail[2] == "" {
			return errors.New("invalid_target")
		}
		if err := client.EnsureRunning(); err != nil {
			return err
		}
		resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/interrupt", map[string]string{"session_id": tail[0], "thread_id": tail[1], "turn_id": tail[2]})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return safeServerError(resp)
		}
		_, err = io.Copy(c.Writer, io.LimitReader(resp.Body, 64<<10))
		return err
	}}
}
