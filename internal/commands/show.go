package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/luthermonson/shipmates/internal/attach"
	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/turninput"
	"github.com/urfave/cli/v3"
)

// Show attaches one or more in-project files — screenshot, log, diff, PDF,
// anything — to a persona.
//
// Every path goes through internal/turninput: confined to the project root,
// no symlinks or reparse points, regular files only, size-capped, and
// revalidated immediately before the bytes are read. Content kind is
// sniffed from the bytes, not the extension.
//
// Delivery depends on whether the persona has a live turn running:
//
//   - live turn in flight → the attachment is injected into that turn, so
//     the crew member sees it without waiting for the turn to end.
//   - otherwise → it dispatches a one-shot turn carrying the attachment,
//     the same shape `ask` produces.
//
// Images ride natively (a local image on the codex turn, a base64 image
// content block on claude). Text files are inlined into the turn text,
// bounded, with an explicit truncation notice. Binary files are referenced
// by project-relative path with size and kind rather than base64-encoded
// into the prompt — both runtimes drive agents that can read a file
// themselves, so the reference is more useful and keeps arbitrary bytes out
// of the context window.
func Show() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "attach a file (screenshot, log, diff, PDF, text) to a persona",
		ArgsUsage: "<persona> <file-path>...",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "caption", Usage: "optional caption sent alongside the file"},
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session instead of resuming (one-shot delivery only)"},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Minute, Usage: "maximum wall-clock duration for a one-shot delivery"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			paths := c.Args().Tail()
			if persona == "" || len(paths) == 0 {
				return errors.New("usage: shipmates show <persona> <file-path>... [--caption <text>]")
			}
			turnCtx, cancel := context.WithTimeout(ctx, c.Duration("timeout"))
			defer cancel()
			return runShow(turnCtx, c.String("runtime"), persona, paths, c.String("caption"), c.Bool("fresh"), c.Writer, c.ErrWriter)
		},
	}
}

func runShow(ctx context.Context, cliRuntime, persona string, paths []string, caption string, fresh bool, stdout, stderr io.Writer) error {
	if persona == "captain" {
		return errors.New("captain is reserved for the human operator; use skipper or quartermaster")
	}
	if err := project.ValidatePersonaName(persona); err != nil {
		return err
	}
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return errors.New("file_root_invalid")
	}
	batch, err := turninput.ValidateFiles(root, paths)
	if err != nil {
		return err
	}
	// The batch is validated here so the operator gets a precise local
	// error; the server revalidates against its own project root before it
	// reads a single byte, so this is convenience, never authority.
	batch.Close()

	if delivered, err := showToLiveSession(ctx, persona, paths, caption, stdout, stderr); err != nil {
		return err
	} else if delivered {
		return nil
	}

	batch, err = turninput.ValidateFiles(root, paths)
	if err != nil {
		return err
	}
	defer batch.Close()
	plan, err := attach.Render(caption, batch.Files(), attach.Native)
	if err != nil {
		return err
	}
	for _, note := range plan.Notes {
		fmt.Fprintln(stderr, "attachment: "+note)
	}
	return dispatchAttachedTurn(ctx, cliRuntime, persona, plan, fresh, stdout, stderr)
}

// showToLiveSession delivers the attachment into the persona's live session
// if one is running. It reports false — without an error — when there is no
// server, or no live session for this persona, so the caller falls back to a
// one-shot turn.
func showToLiveSession(ctx context.Context, persona string, paths []string, caption string, stdout, stderr io.Writer) (bool, error) {
	if !client.Healthy() {
		return false, nil
	}
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/show", map[string]any{"files": paths, "caption": caption})
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		code := serverErrorCode(resp)
		switch livesession.Code(code) {
		case livesession.NotFound, livesession.StoppedCode, livesession.FailedCode:
			// No live session to attach to — one-shot delivery it is.
			return false, nil
		}
		return false, errors.New(code)
	}
	var res livesession.ShowResult
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&res) != nil {
		return false, errors.New("invalid show server response")
	}
	for _, note := range res.Notes {
		fmt.Fprintln(stderr, "attachment: "+note)
	}
	target := "a new turn"
	if res.Injected {
		target = "the running turn"
	}
	fmt.Fprintf(stderr, "delivered to %s's live session (%s); watch with: shipmates feed %s --follow\n", persona, target, persona)
	return true, json.NewEncoder(stdout).Encode(res.Snapshot)
}

// dispatchAttachedTurn sends a one-shot turn carrying the rendered
// attachment plan, through whichever runtime the selector resolves — the
// same routing `ask` uses.
func dispatchAttachedTurn(ctx context.Context, cliRuntime, persona string, plan attach.Plan, fresh bool, stdout, stderr io.Writer) error {
	rt, source, err := selectAskRuntime(ctx, cliRuntime)
	if err != nil {
		return err
	}
	if rt == nil {
		installed, err := project.CanonicalPersonaAt(".", persona)
		if err != nil {
			return err
		}
		cfg, err := project.ResolvePersonaConfig(persona)
		if err != nil {
			return err
		}
		if err := turninput.RevalidateDescriptors(plan.Images); err != nil {
			return err
		}
		return codexTurnDispatcher(ctx, installed, plan.Text, fresh, cfg, plan.Images, stdout, stderr)
	}
	defer rt.Close(ctx)
	if len(plan.Images) > 0 && !rt.Capabilities().Attachments {
		return fmt.Errorf("runtime %s cannot carry image attachments (source: %s)", rt.Name(), source)
	}
	attachments, err := attach.RuntimeAttachments(plan.Images)
	if err != nil {
		return err
	}
	return dispatchRuntimeTurn(ctx, rt, persona, plan.Text, fresh, attachments, stdout, stderr)
}
