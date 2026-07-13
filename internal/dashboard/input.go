package dashboard

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/livesession"
)

const MaxInputBytes = 16 << 10

type InputKind uint8

const (
	InputLine InputKind = iota
	InputResize
	InputEOF
	InputCancel
)

type Input struct {
	Kind InputKind
	Line string
	Size Size
}

// Editor is fakeable and yields only completed local input events. Drafts are
// deliberately absent from the dashboard model and renderer.
type Editor interface {
	Next(context.Context) (Input, error)
}

type ParsedKind uint8

const (
	ParsedNoop ParsedKind = iota
	ParsedMessage
	ParsedHelp
	ParsedInterrupt
	ParsedDetach
	ParsedAllowOnce
	ParsedDeny
	ParsedImageAdd
	ParsedImageRemove
	ParsedImageClear
	ParsedUnknown
)

type ParsedInput struct {
	Kind ParsedKind
	Text string
}

var ErrInvalidInput = errors.New("invalid_input")
var ErrInputTooLarge = errors.New("input_too_large")

func ParseLine(line string) (ParsedInput, error) {
	if !utf8.ValidString(line) || strings.IndexFunc(line, func(r rune) bool { return r < 0x20 && r != '\t' }) >= 0 {
		return ParsedInput{}, ErrInvalidInput
	}
	if len(line) > MaxInputBytes {
		return ParsedInput{}, ErrInputTooLarge
	}
	if line == "" {
		return ParsedInput{Kind: ParsedNoop}, nil
	}
	if strings.HasPrefix(line, "//") {
		return ParsedInput{Kind: ParsedMessage, Text: line[1:]}, nil
	}
	if strings.HasPrefix(line, "/image add ") {
		p := strings.TrimSpace(strings.TrimPrefix(line, "/image add "))
		if p == "" {
			return ParsedInput{}, ErrInvalidInput
		}
		return ParsedInput{Kind: ParsedImageAdd, Text: p}, nil
	}
	if strings.HasPrefix(line, "/image remove ") {
		p := strings.TrimSpace(strings.TrimPrefix(line, "/image remove "))
		if _, e := strconv.Atoi(p); e != nil {
			return ParsedInput{}, ErrInvalidInput
		}
		return ParsedInput{Kind: ParsedImageRemove, Text: p}, nil
	}
	if line == "/image clear" {
		return ParsedInput{Kind: ParsedImageClear}, nil
	}
	switch line {
	case "/help":
		return ParsedInput{Kind: ParsedHelp}, nil
	case "/interrupt":
		return ParsedInput{Kind: ParsedInterrupt}, nil
	case "/detach", "/quit":
		return ParsedInput{Kind: ParsedDetach}, nil
	case "/allow-once":
		return ParsedInput{Kind: ParsedAllowOnce}, nil
	case "/deny":
		return ParsedInput{Kind: ParsedDeny}, nil
	}
	if strings.HasPrefix(line, "/") {
		return ParsedInput{Kind: ParsedUnknown}, nil
	}
	return ParsedInput{Kind: ParsedMessage, Text: line}, nil
}

// InputLoop establishes resize/cancel/EOF behavior without wiring mutating
// dashboard actions. onLine receives parsed local intent for a later task.
func InputLoop(ctx context.Context, editor Editor, initial Size, render func(Screen) error, model *Model, onLine func(ParsedInput) error) error {
	size := initial
	if err := render(Render(model, size)); err != nil {
		return err
	}
	for {
		in, err := editor.Next(ctx)
		if err != nil {
			return err
		}
		switch in.Kind {
		case InputResize:
			size = in.Size
			if err := render(Render(model, size)); err != nil {
				return err
			}
		case InputEOF, InputCancel:
			return nil
		case InputLine:
			p, err := ParseLine(in.Line)
			if err != nil {
				return err
			}
			if p.Kind == ParsedDetach {
				return nil
			}
			if onLine != nil {
				if err := onLine(p); err != nil {
					return err
				}
			}
		default:
			return ErrInvalidInput
		}
	}
}

// ActionLoop wires parsed submitted lines to exact controller-scoped actions.
// Heartbeats run independently of blocked local input; cancellation and EOF
// remain detach-only and Close is owned by the caller's restoration path.
func ActionLoop(ctx context.Context, editor Editor, initial Size, render func(Screen) error, model *Model, conn *Connection) error {
	lease := time.Duration(conn.Attach.LeaseTimeoutMS) * time.Millisecond
	if lease <= 0 {
		lease = 15 * time.Second
	}
	ticker := time.NewTicker(lease / 3)
	defer ticker.Stop()
	return ActionLoopWithTicks(ctx, editor, initial, render, model, conn, ticker.C)
}

// ActionLoopWithTicks exposes lease/feed ticks for deterministic fake-clock
// integration tests; production uses ActionLoop's bounded lease cadence.
func ActionLoopWithTicks(ctx context.Context, editor Editor, initial Size, render func(Screen) error, model *Model, conn *Connection, ticks <-chan time.Time) error {
	defer model.ClearImages()
	size := initial
	redraw := func() error { return render(Render(model, size)) }
	if err := redraw(); err != nil {
		return err
	}
	inCh := make(chan Input, 1)
	errCh := make(chan error, 1)
	go func() {
		for {
			in, err := editor.Next(ctx)
			if err != nil {
				select {
				case errCh <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case inCh <- in:
			case <-ctx.Done():
				return
			}
			if in.Kind == InputEOF || in.Kind == InputCancel {
				return
			}
		}
	}()
	for {
		var in Input
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			after := uint64(0)
			if model.next > 0 {
				after = model.next - 1
			}
			synced, err := conn.Sync(ctx, after)
			if err != nil {
				return err
			}
			conn.Attach = synced
			if err := model.Reconnect(synced); err != nil {
				return err
			}
			if err := redraw(); err != nil {
				return err
			}
			continue
		case err := <-errCh:
			return err
		case in = <-inCh:
		}
		switch in.Kind {
		case InputEOF, InputCancel:
			return nil
		case InputResize:
			size = in.Size
			if err := redraw(); err != nil {
				return err
			}
			continue
		case InputLine:
		default:
			return ErrInvalidInput
		}
		p, err := ParseLine(in.Line)
		if err != nil {
			model.Notice(err.Error())
			if err := redraw(); err != nil {
				return err
			}
			continue
		}
		switch p.Kind {
		case ParsedNoop:
			continue
		case ParsedDetach:
			return nil
		case ParsedHelp:
			model.Notice("/help  /interrupt  /detach  /quit  // literal slash; pending only: /allow-once /deny")
		case ParsedUnknown:
			model.Notice("unknown_command; use /help")
		case ParsedImageAdd:
			if model.State != livesession.Idle || model.PendingApproval != nil {
				model.ClearImages()
				model.Notice("image_wrong_state")
				break
			}
			if err := model.AddImage(p.Text); err != nil {
				model.Notice(err.Error())
			}
		case ParsedImageRemove:
			i, _ := strconv.Atoi(p.Text)
			if err := model.RemoveImage(i); err != nil {
				model.Notice(err.Error())
			}
		case ParsedImageClear:
			model.ClearImages()
			model.Notice("images cleared")
		case ParsedAllowOnce, ParsedDeny:
			card := model.PendingApproval
			if card == nil {
				model.Notice("stale/already resolved")
				break
			}
			decision := livesession.AllowOnce
			if p.Kind == ParsedDeny {
				decision = livesession.DenyOnce
			}
			res, actionErr := conn.ResolveApproval(ctx, card, decision)
			if actionErr != nil {
				if actionErr.Error() == string(livesession.StaleApproval) || actionErr.Error() == string(livesession.AlreadyResolved) || actionErr.Error() == string(livesession.StaleController) {
					model.Notice("stale/already resolved")
				} else {
					model.Notice(actionErr.Error())
				}
			} else {
				switch res.Outcome {
				case "allowed_once":
					model.Notice("allowed once")
				case "denied":
					model.Notice("denied")
				case "resolution_failed":
					model.Notice("resolution_failed")
				default:
					model.Notice("stale/already resolved")
				}
				model.PendingApproval = nil
				conn.Attach.PendingApproval = nil
			}
		case ParsedMessage, ParsedInterrupt:
			if model.PendingApproval != nil && p.Kind == ParsedMessage {
				model.Notice("approval_pending")
				break
			}
			action := livesession.ControllerMessage
			if p.Kind == ParsedInterrupt {
				action = livesession.ControllerInterrupt
			}
			snap := livesession.Snapshot{Persona: model.Persona, SessionID: conn.Attach.SessionID, Backend: conn.Attach.Backend, ThreadID: conn.Attach.ThreadID, TurnID: conn.Attach.TurnID, State: model.State}
			images := model.ImagePaths()
			if action != livesession.ControllerMessage || snap.State != livesession.Idle {
				images = nil
			}
			res, actionErr := conn.ActionImages(ctx, action, snap, p.Text, images)
			if actionErr != nil {
				model.ClearImages()
				model.Notice(actionErr.Error())
			} else {
				model.ClearImages()
				conn.Attach.Snapshot = res.Snapshot
				if err := model.ApplySnapshot(res.Snapshot); err != nil {
					return err
				}
				switch {
				case action == livesession.ControllerMessage && snap.State == livesession.Idle:
					model.Notice("turn started")
				case action == livesession.ControllerMessage:
					model.Notice("steer accepted")
				case res.Code == livesession.AlreadyInterrupting:
					model.Notice("already interrupting")
				case res.Code == livesession.AlreadyTerminal:
					model.Notice("already idle")
				default:
					model.Notice("interrupt accepted")
				}
			}
		}
		if err := redraw(); err != nil {
			return err
		}
	}
}
