// Package attach renders validated file attachments into the two shapes
// the runtimes accept, from a single runtime-neutral plan:
//
//   - Text: the prompt text an operator's attachment turns into. Text files
//     are inlined (bounded, with an explicit truncation notice); binary
//     files are referenced by project-relative path with size and kind
//     metadata rather than base64-encoded into the prompt.
//   - Images: the raster descriptors, passed through to whichever transport
//     the runtime uses — codex takes local image paths on the turn, claude
//     takes base64 image content blocks.
//
// Binary non-image files (PDFs, archives, executables) are deliberately
// never inlined. Both runtimes drive agents that have a file-read tool, so
// a path reference is strictly more useful than a wall of base64 the model
// cannot decode, and it keeps an arbitrarily-shaped byte stream out of the
// prompt. The rendered text says so explicitly, so the agent knows to read
// the file itself.
package attach

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/turninput"
)

// MaxInlineTextBytes bounds how much of one text attachment is inlined into
// the prompt. Larger files are truncated with a notice naming the path so
// the agent can read the rest itself.
const MaxInlineTextBytes = 64 * 1024

// MaxTotalInlineTextBytes bounds the inlined text across a whole batch.
const MaxTotalInlineTextBytes = 128 * 1024

// Mode selects how images are represented.
type Mode int

const (
	// Native attaches images to the turn itself: a localImage input on a
	// codex turn, a base64 image content block on claude. Plan.Images
	// carries the descriptors.
	Native Mode = iota
	// Reference names images by project-relative path in the text instead.
	// Used for transports that cannot carry an image with the message —
	// today that is a codex mid-turn steer, whose input shipmates only
	// sends as text. Plan.Images is empty in this mode.
	Reference
)

// Plan is the runtime-neutral rendering of one attachment batch.
type Plan struct {
	// Text is the complete turn text: caption, per-file headers, inlined
	// text content and binary path references.
	Text string
	// Images are the raster descriptors to hand to the runtime's own image
	// transport. Never contains text or binary attachments.
	Images []turninput.FileDescriptorV1
	// Notes are operator-facing one-liners about what happened to each
	// attachment (inlined, truncated, referenced by path).
	Notes []string
}

// Render builds the plan for a caption plus a set of already-validated
// descriptors. It reads text attachments through FileDescriptorV1.Bytes,
// which revalidates identity before and after the read, so a file swapped
// since validation is refused rather than inlined.
func Render(caption string, files []turninput.FileDescriptorV1, mode Mode) (Plan, error) {
	if len(files) == 0 {
		return Plan{Text: strings.TrimSpace(caption)}, nil
	}
	var b strings.Builder
	plan := Plan{}
	noun := "file"
	if len(files) > 1 {
		noun = "files"
	}
	fmt.Fprintf(&b, "[attachment] the operator attached %d %s:\n", len(files), noun)
	var inlined int
	for _, d := range files {
		switch d.Kind {
		case turninput.FileImage:
			if mode == Reference {
				fmt.Fprintf(&b, "\n- %s (%s image, %d bytes) — not attached inline. Open it from the project at that path to view it.\n", d.DisplayPath(), d.Format, d.Size)
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s: %s image, referenced by path (this transport cannot attach it mid-turn)", d.DisplayPath(), d.Format))
				break
			}
			fmt.Fprintf(&b, "\n- %s (%s image, %d bytes) — attached to this turn as an image.\n", d.DisplayPath(), d.Format, d.Size)
			plan.Images = append(plan.Images, d)
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s: attached as %s image", d.DisplayPath(), d.Format))
		case turninput.FileText:
			raw, err := d.Bytes()
			if err != nil {
				return Plan{}, fmt.Errorf("read %s: %w", d.DisplayPath(), err)
			}
			budget := MaxInlineTextBytes
			if remaining := MaxTotalInlineTextBytes - inlined; remaining < budget {
				budget = remaining
			}
			if budget < 0 {
				budget = 0
			}
			body, truncated := boundedUTF8(string(raw), budget)
			inlined += len(body)
			fmt.Fprintf(&b, "\n- %s (text, %d bytes):\n", d.DisplayPath(), d.Size)
			fmt.Fprintf(&b, "%s\n%s\n%s\n", fence(body), body, fence(body))
			if truncated {
				fmt.Fprintf(&b, "(truncated to %d of %d bytes — read %s for the rest)\n", len(body), d.Size, d.DisplayPath())
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s: inlined %d of %d bytes (truncated)", d.DisplayPath(), len(body), d.Size))
			} else {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s: inlined %d bytes of text", d.DisplayPath(), d.Size))
			}
		default:
			fmt.Fprintf(&b, "\n- %s (binary, %d bytes) — not inlined. Read it from the project at that path if you need its contents.\n", d.DisplayPath(), d.Size)
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s: binary, referenced by path (not inlined)", d.DisplayPath()))
		}
	}
	if c := strings.TrimSpace(caption); c != "" {
		fmt.Fprintf(&b, "\n%s\n", c)
	}
	plan.Text = b.String()
	return plan, nil
}

// RuntimeAttachments materializes image descriptors into runtime.Attachment
// values with their bytes and media type, for runtimes that carry images as
// inline content blocks. Call it immediately before dispatching the turn:
// every Bytes() read revalidates the descriptor first.
func RuntimeAttachments(files []turninput.FileDescriptorV1) ([]runtime.Attachment, error) {
	out := make([]runtime.Attachment, 0, len(files))
	for _, d := range files {
		if d.Kind != turninput.FileImage {
			// Non-images are already represented in Plan.Text; they never
			// become content blocks.
			continue
		}
		raw, err := d.Bytes()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", d.DisplayPath(), err)
		}
		out = append(out, runtime.Attachment{
			Path:        d.AbsolutePath(),
			DisplayPath: d.DisplayPath(),
			Kind:        string(turninput.FileImage),
			MediaType:   d.Format.MediaType(),
			Size:        d.Size,
			Base64:      base64.StdEncoding.EncodeToString(raw),
		})
	}
	return out, nil
}

// boundedUTF8 truncates at a rune boundary and reports whether it cut.
func boundedUTF8(s string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= limit {
		return s, false
	}
	for limit > 0 && s[limit]&0xc0 == 0x80 {
		limit--
	}
	return s[:limit], true
}

// fence picks a code fence long enough that the body cannot terminate it.
func fence(body string) string {
	f := "```"
	for strings.Contains(body, f) {
		f += "`"
	}
	return f
}
