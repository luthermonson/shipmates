package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

// AttachResponse is the JSON body returned by POST /attach on the ship and by
// POST /api/captain/{key}/attach on Fleet Command — the shared success shape
// the UI parses.
type AttachResponse struct {
	AttachID string `json:"attachId"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

// attachInboxDirName is the on-disk directory (relative to the project root)
// where inbound binary attachments land. Deliberately under `.shipmates/` so
// it lives alongside vendored persona state and can be swept in one glob.
const attachInboxDirName = "inbox"

// attachMaxBytes is the hard cap on a single upload's body. 10 MB matches the
// contract; the parser is given a slightly larger budget (attachParseLimit)
// so a well-formed 10 MB file with multipart framing still fits.
const attachMaxBytes = int64(10 << 20)

// attachParseLimit is the ParseMultipartForm budget: 11 MB gives us headroom
// for the multipart boundary/headers on a right-at-the-cap file without
// falsely rejecting it.
const attachParseLimit = int64(11 << 20)

// attachCaptionMaxBytes bounds the caption field so someone can't smuggle log
// data through it. 2 KB is plenty for an image caption.
const attachCaptionMaxBytes = 2 << 10

// attachSweepInterval and attachTTL configure the inbox sweeper: files older
// than attachTTL (by mtime) are deleted every attachSweepInterval. No config
// knobs — kept simple; a future PR can add per-project overrides.
const (
	attachSweepInterval = time.Hour
	attachTTL           = 7 * 24 * time.Hour
)

// allowedAttachExts is the closed set of extensions the ship will accept.
// Anything else — including arbitrary content types the client claims — is
// rejected with 400. Keeping this closed prevents the inbox from becoming a
// staging ground for executables written into the project checkout.
var allowedAttachExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".webp": true,
	".heic": true,
	".pdf":  true,
	".txt":  true,
}

// contentTypeToExt maps common client-declared MIME types to a canonical file
// extension. Populated only for the allowedAttachExts set (plus a couple of
// legacy JPEG aliases). Returns "" for anything not in this table so the
// caller can fall through to filename / byte sniffing.
var contentTypeToExt = map[string]string{
	"image/jpeg":    ".jpg",
	"image/jpg":     ".jpg",
	"image/pjpeg":   ".jpg",
	"image/png":     ".png",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/heic":    ".heic",
	"image/heif":    ".heic",
	"application/pdf": ".pdf",
	"text/plain":    ".txt",
}

// handleAttach receives a multipart POST with a single `file` field, writes it
// into <projectRoot>/.shipmates/inbox/<uuid>.<ext>, emits an attach:received
// event, and returns the AttachResponse JSON. Rejects requests over the size
// cap (400), missing files (400), and unrecognized content (400).
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	// Cap the raw body first so a client streaming a 5 GB PDF can't chew
	// through the multipart parser's temp-file spill before we bail.
	r.Body = http.MaxBytesReader(w, r.Body, attachParseLimit)
	if err := r.ParseMultipartForm(attachParseLimit); err != nil {
		if _, ok := err.(*http.MaxBytesError); ok {
			writeAttachError(w, http.StatusBadRequest, "attachment exceeds 10 MB limit")
			return
		}
		writeAttachError(w, http.StatusBadRequest, "bad multipart body: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeAttachError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()

	if header.Size > attachMaxBytes {
		writeAttachError(w, http.StatusBadRequest, "attachment exceeds 10 MB limit")
		return
	}

	// Content-Type precedence: the form field's declared type wins because
	// mobile clients set it accurately from the picker; then filename
	// extension (still usually right); then byte sniffing on the first 512
	// bytes as a last resort. The path that won is logged at debug.
	ext, source := detectAttachExtParts(file, header.Filename, header.Header.Get("Content-Type"))
	if ext == "" || !allowedAttachExts[ext] {
		writeAttachError(w, http.StatusBadRequest, "unsupported attachment type")
		return
	}
	slog.Debug("attach: detected extension", "ext", ext, "source", source, "filename", header.Filename)

	// Rewind: detectAttachExt may have consumed the sniff bytes. FormFile
	// returns a *multipart.FileHeader-backed file that implements io.Seeker,
	// so this is safe.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeAttachError(w, http.StatusInternalServerError, "rewind: "+err.Error())
		return
	}

	// Caption is optional; enforce a size ceiling before we quote it back.
	caption := strings.TrimSpace(r.FormValue("caption"))
	if len(caption) > attachCaptionMaxBytes {
		writeAttachError(w, http.StatusBadRequest, "caption exceeds 2 KB limit")
		return
	}

	inboxDir := s.attachInboxDir()
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		writeAttachError(w, http.StatusInternalServerError, "mkdir inbox: "+err.Error())
		return
	}

	attachID := project.NewUUID()[:8]
	filename := attachID + ext
	absPath := filepath.Join(inboxDir, filename)

	dst, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		writeAttachError(w, http.StatusInternalServerError, "create file: "+err.Error())
		return
	}
	written, copyErr := io.Copy(dst, io.LimitReader(file, attachMaxBytes+1))
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(absPath)
		errMsg := "write file"
		if copyErr != nil {
			errMsg += ": " + copyErr.Error()
		} else {
			errMsg += ": " + closeErr.Error()
		}
		writeAttachError(w, http.StatusInternalServerError, errMsg)
		return
	}
	if written > attachMaxBytes {
		_ = os.Remove(absPath)
		writeAttachError(w, http.StatusBadRequest, "attachment exceeds 10 MB limit")
		return
	}

	// Path returned to callers is always the repo-relative form so the UI
	// and the auto-tell can render it verbatim.
	relPath := filepath.ToSlash(filepath.Join(project.Dir, attachInboxDirName, filename))

	text := "received " + relPath
	if caption != "" {
		text += " — " + caption
	}
	s.addEvent(Event{Type: "attach:received", Text: text, Input: relPath})

	resp := AttachResponse{AttachID: attachID, Path: relPath, Size: written}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// attachInboxDir returns the absolute path to the ship's inbox. Anchored on
// s.projectRoot (captured at New()) rather than os.Getwd() so a chdir mid-
// process can't redirect writes outside the checkout.
func (s *Server) attachInboxDir() string {
	root := s.projectRoot
	if root == "" {
		root, _ = os.Getwd()
	}
	return filepath.Join(root, project.Dir, attachInboxDirName)
}

// detectAttachExtParts runs the Content-Type -> filename -> byte-sniff
// precedence described in handleAttach, keyed off the concrete strings the
// multipart layer hands us. The second return value tells the caller which
// rung won (for the debug log). The file position may advance during
// sniffing; callers must Seek back to 0 before writing.
func detectAttachExtParts(file io.ReadSeeker, filename, contentType string) (string, string) {
	// 1. Declared Content-Type
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		if ext, ok := contentTypeToExt[strings.ToLower(mediaType)]; ok {
			return ext, "content-type"
		}
	}

	// 2. Filename extension
	if filename != "" {
		if ext := strings.ToLower(filepath.Ext(filename)); allowedAttachExts[ext] {
			// Normalize .jpeg -> .jpg? No — keep the client's choice, both
			// are in the allow-list and the UI won't care.
			return ext, "filename"
		}
	}

	// 3. Byte sniff. http.DetectContentType always inspects the first 512
	// bytes, so a short file just gets padded internally.
	head := make([]byte, 512)
	n, _ := file.Read(head)
	if n <= 0 {
		return "", "empty"
	}
	sniffed := http.DetectContentType(head[:n])
	mediaType, _, err := mime.ParseMediaType(sniffed)
	if err != nil {
		return "", "sniff-parse-error"
	}
	if ext, ok := contentTypeToExt[strings.ToLower(mediaType)]; ok {
		return ext, "sniff"
	}
	return "", "sniff-unknown"
}

// writeAttachError writes a JSON {"error": "..."} body with the given HTTP
// status. Matches the shape the UI expects across every /api error path.
func writeAttachError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// attachSweeperLoop runs in the background from server start, deleting inbox
// files whose mtime is older than attachTTL. Fails soft — every error is
// logged at warn level, the loop keeps running.
func (s *Server) attachSweeperLoop(ctx context.Context) {
	// Run once at startup so a restart doesn't wait an hour to clean up
	// files that already crossed the TTL.
	s.attachSweepOnce(time.Now())
	ticker := time.NewTicker(attachSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			s.attachSweepOnce(now)
		}
	}
}

// attachSweepOnce deletes every file in the inbox whose mtime is older than
// attachTTL (relative to `now`). Split out from the loop so tests can drive
// it with a synthetic clock.
func (s *Server) attachSweepOnce(now time.Time) {
	dir := s.attachInboxDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("attach sweeper: readdir failed", "dir", dir, "err", err)
		}
		return
	}
	cutoff := now.Add(-attachTTL)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("attach sweeper: stat failed", "file", e.Name(), "err", err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if err := os.Remove(full); err != nil {
			slog.Warn("attach sweeper: remove failed", "file", full, "err", err)
			continue
		}
		slog.Info("attach sweeper: removed stale file", "file", full, "age", now.Sub(info.ModTime()).Round(time.Minute))
	}
}

