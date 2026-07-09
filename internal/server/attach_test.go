package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAttachTestServer wires up a Server rooted at a fresh temp directory and
// exposes handleAttach through an httptest server. The temp dir plays the
// role of the ship's project checkout — the inbox will land inside it.
func newAttachTestServer(t *testing.T) (*Server, string, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	s := New()
	s.projectRoot = root // override so the sniff doesn't hit the real repo
	mux := http.NewServeMux()
	mux.HandleFunc("POST /attach", s.handleAttach)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, root, ts
}

// buildAttachRequest wraps a payload in a multipart body with the given field
// content type and filename. Returns the request body and its final Content-
// Type header (the multipart boundary is embedded).
func buildAttachRequest(t *testing.T, filename, contentType string, payload []byte, caption string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	if contentType != "" {
		hdr["Content-Type"] = []string{contentType}
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if caption != "" {
		if err := mw.WriteField("caption", caption); err != nil {
			t.Fatalf("caption field: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf, mw.FormDataContentType()
}

// pngBytes is a minimal PNG stub — the 8-byte signature is enough for
// http.DetectContentType to identify image/png without embedding a real
// image in the test.
var pngBytes = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}

func TestAttachHappyPath(t *testing.T) {
	s, root, ts := newAttachTestServer(t)

	body, ctype := buildAttachRequest(t, "shot.png", "image/png", pngBytes, "look at this")
	req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
	req.Header.Set("Content-Type", ctype)

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}

	var out AttachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.AttachID == "" || out.Path == "" || out.Size != int64(len(pngBytes)) {
		t.Fatalf("bad response: %+v", out)
	}
	if !strings.HasSuffix(out.Path, ".png") {
		t.Fatalf("expected .png suffix, got %q", out.Path)
	}
	if !strings.HasPrefix(out.Path, ".shipmates/inbox/") {
		t.Fatalf("expected .shipmates/inbox/ prefix, got %q", out.Path)
	}

	// File must actually be on disk with matching bytes.
	full := filepath.Join(root, filepath.FromSlash(out.Path))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read landed file: %v", err)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Fatalf("file content mismatch")
	}

	// Event should have been emitted.
	s.mu.Lock()
	defer s.mu.Unlock()
	var seen bool
	for _, e := range s.events {
		if e.Type == "attach:received" && strings.Contains(e.Text, out.Path) && strings.Contains(e.Text, "look at this") {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("expected attach:received event with path and caption, events=%+v", s.events)
	}
}

func TestAttachContentTypePrecedenceUsesFormField(t *testing.T) {
	_, root, ts := newAttachTestServer(t)
	// Filename says .bin (rejected extension), Content-Type says PDF (allowed).
	// The form field's Content-Type must win.
	body, ctype := buildAttachRequest(t, "weird.bin", "application/pdf", []byte("%PDF-1.4 ..."), "")
	req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}
	var out AttachResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.HasSuffix(out.Path, ".pdf") {
		t.Fatalf("expected .pdf via content-type precedence, got %q", out.Path)
	}
	// And confirm it landed.
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(out.Path))); err != nil {
		t.Fatalf("landed file missing: %v", err)
	}
}

func TestAttachContentTypeSniffFallback(t *testing.T) {
	_, _, ts := newAttachTestServer(t)
	// No filename extension, blank Content-Type: byte sniffing on the PNG
	// magic bytes must save it.
	body, ctype := buildAttachRequest(t, "noext", "", pngBytes, "")
	req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}
	var out AttachResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.HasSuffix(out.Path, ".png") {
		t.Fatalf("expected .png via byte sniff, got %q", out.Path)
	}
}

func TestAttachTooLarge(t *testing.T) {
	_, _, ts := newAttachTestServer(t)
	// One byte past the 10 MB cap. attachParseLimit is 11 MB so the
	// multipart parser accepts the framing, but the size check rejects.
	big := bytes.Repeat([]byte{0xFF}, int(attachMaxBytes)+1)
	body, ctype := buildAttachRequest(t, "big.png", "image/png", big, "")
	req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestAttachUnsupportedType(t *testing.T) {
	_, _, ts := newAttachTestServer(t)
	// Executable-ish payload with an executable-ish Content-Type and no
	// allowed filename extension. All three detection rungs should refuse.
	// The magic bytes here (Mach-O feedfacf) sniff to application/octet-
	// stream, which isn't in our allow map.
	body, ctype := buildAttachRequest(t, "run.exe", "application/x-msdownload", []byte{0xCF, 0xFA, 0xED, 0xFE, 0x07, 0x00, 0x00, 0x01, 0x03, 0x00}, "")
	req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestAttachMissingFile(t *testing.T) {
	_, _, ts := newAttachTestServer(t)
	// Multipart with only a caption, no file field.
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("caption", "just a caption")
	_ = mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/attach", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestAttachCaptionTooLarge(t *testing.T) {
	_, _, ts := newAttachTestServer(t)
	caption := strings.Repeat("x", attachCaptionMaxBytes+1)
	body, ctype := buildAttachRequest(t, "shot.png", "image/png", pngBytes, caption)
	req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestAttachSweeperDeletesStale(t *testing.T) {
	// Table-driven: for each row we pre-seed the inbox with a file whose
	// mtime is offset from `now` by `age`, then run one sweep and check
	// whether the file survived.
	cases := []struct {
		name     string
		age      time.Duration
		wantGone bool
	}{
		{"fresh", time.Minute, false},
		{"one hour old", time.Hour, false},
		{"six days", 6 * 24 * time.Hour, false},
		{"eight days old (past TTL)", 8 * 24 * time.Hour, true},
		{"thirty days old", 30 * 24 * time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s := New()
			s.projectRoot = root
			inbox := s.attachInboxDir()
			if err := os.MkdirAll(inbox, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(inbox, "test.png")
			if err := os.WriteFile(file, pngBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			// Backdate the mtime to `age` before "now".
			mtime := now.Add(-tc.age)
			if err := os.Chtimes(file, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			s.attachSweepOnce(now)
			_, err := os.Stat(file)
			gone := os.IsNotExist(err)
			if gone != tc.wantGone {
				t.Fatalf("age %s: wantGone=%v, gotGone=%v (err=%v)", tc.age, tc.wantGone, gone, err)
			}
		})
	}
}

func TestAttachSweeperMissingDirNoOp(t *testing.T) {
	// A ship that never received an attach has no inbox — the sweeper must
	// tolerate this silently (no panic, no log spam).
	root := t.TempDir()
	s := New()
	s.projectRoot = root
	// Do NOT create the inbox. Sweep should return cleanly.
	s.attachSweepOnce(time.Now())
}
