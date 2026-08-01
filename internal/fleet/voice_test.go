package fleet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// /api/voice/config
// ---------------------------------------------------------------------------

func TestHandleVoiceConfig(t *testing.T) {
	cases := []struct {
		name string
		conv *convConfig
		want map[string]bool
	}{
		{"no voice at all", nil, map[string]bool{"conversation": false, "tts": false, "stt": false}},
		{"llm only", &convConfig{url: "http://x/v1"}, map[string]bool{"conversation": true, "tts": false, "stt": false}},
		{"claude backend counts as conversation", &convConfig{brain: newClaudeBrain("", "", "")},
			map[string]bool{"conversation": true, "tts": false, "stt": false}},
		{"edge voice enables tts", &convConfig{voice: "en-US-AriaNeural"},
			map[string]bool{"conversation": false, "tts": true, "stt": false}},
		{"tts url enables tts", &convConfig{ttsURL: "http://x/v1/audio/speech"},
			map[string]bool{"conversation": false, "tts": true, "stt": false}},
		{"stt url enables stt", &convConfig{sttURL: "http://x/inference"},
			map[string]bool{"conversation": false, "tts": false, "stt": true}},
		{"everything", &convConfig{url: "http://x/v1", voice: "af_heart", sttURL: "http://x/inference"},
			map[string]bool{"conversation": true, "tts": true, "stt": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Server{conv: tc.conv}
			rec := httptest.NewRecorder()
			b.handleVoiceConfig(rec, httptest.NewRequest("GET", "/api/voice/config", nil))
			got := decodeJSON[map[string]bool](t, rec.Body.Bytes())
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %v, want %v (full: %+v)", k, got[k], want, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /api/stt
// ---------------------------------------------------------------------------

// fakeSTT is a whisper.cpp / OpenAI-compatible transcription server.
type fakeSTT struct {
	srv *httptest.Server

	mu     sync.Mutex
	fields map[string]string
	file   []byte
	body   string
	status int
}

func newFakeSTT(t *testing.T, body string) *fakeSTT {
	t.Helper()
	f := &fakeSTT{fields: map[string]string{}, body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err == nil {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				f.mu.Lock()
				if part.FileName() != "" {
					f.file = data
					f.fields["__filename"] = part.FileName()
				} else {
					f.fields[part.FormName()] = string(data)
				}
				f.mu.Unlock()
			}
		}
		f.mu.Lock()
		status, out := f.status, f.body
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newSTTFleet(url string) *Server {
	return &Server{conv: &convConfig{sttURL: url, client: &http.Client{Timeout: 30 * time.Second}}}
}

func TestHandleSTT_NotConfigured(t *testing.T) {
	for _, b := range []*Server{{}, {conv: &convConfig{}}} {
		rec := httptest.NewRecorder()
		b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", strings.NewReader("audio")))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("want 501 when --stt-url is absent, got %d", rec.Code)
		}
	}
}

func TestHandleSTT_EmptyBodyIs400(t *testing.T) {
	b := newSTTFleet("http://unused.invalid")
	rec := httptest.NewRecorder()
	b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", strings.NewReader("")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an empty body, got %d", rec.Code)
	}
}

func TestHandleSTT_OversizeIs413(t *testing.T) {
	b := newSTTFleet("http://unused.invalid")
	rec := httptest.NewRecorder()
	b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", bytes.NewReader(make([]byte, sttMaxBytes+1))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 for an oversize utterance, got %d", rec.Code)
	}
}

// The audio has to reach the server as a multipart file part, with the model
// field OAI-compatible servers require. whisper.cpp also embeds "\n " at its
// segment boundaries — speech has no line structure, so it must be collapsed.
func TestHandleSTT_HappyPath(t *testing.T) {
	stt := newFakeSTT(t, `{"text":"\n hello there \n how are you\n"}`)
	b := newSTTFleet(stt.srv.URL)
	b.conv.sttModel = "whisper-1"

	audio := []byte("RIFF....WAVEfmt ")
	rec := httptest.NewRecorder()
	b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", bytes.NewReader(audio)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeJSON[map[string]string](t, rec.Body.Bytes())
	if got["text"] != "hello there how are you" {
		t.Fatalf("transcript not normalized: %q", got["text"])
	}

	stt.mu.Lock()
	defer stt.mu.Unlock()
	if !bytes.Equal(stt.file, audio) {
		t.Errorf("audio not forwarded verbatim: %q", stt.file)
	}
	if stt.fields["__filename"] != "utterance.wav" {
		t.Errorf("filename %q — whisper.cpp keys off the extension", stt.fields["__filename"])
	}
	if stt.fields["model"] != "whisper-1" {
		t.Errorf("model field not sent: %q", stt.fields["model"])
	}
	if stt.fields["response_format"] != "json" {
		t.Errorf("response_format not sent: %q", stt.fields["response_format"])
	}
}

// whisper.cpp ignores the model field; when the operator didn't configure one
// we must not send an empty string (some OAI servers 422 on it).
func TestHandleSTT_OmitsEmptyModel(t *testing.T) {
	stt := newFakeSTT(t, `{"text":"hi"}`)
	b := newSTTFleet(stt.srv.URL)

	rec := httptest.NewRecorder()
	b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", strings.NewReader("audio")))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	stt.mu.Lock()
	defer stt.mu.Unlock()
	if _, present := stt.fields["model"]; present {
		t.Errorf("model field should be omitted when unset")
	}
}

func TestHandleSTT_UpstreamFailures(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		stt := newFakeSTT(t, "model missing")
		stt.status = http.StatusInternalServerError
		b := newSTTFleet(stt.srv.URL)
		rec := httptest.NewRecorder()
		b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", strings.NewReader("audio")))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("want 502, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "model missing") {
			t.Errorf("upstream detail lost: %q", rec.Body.String())
		}
	})
	t.Run("non-json response", func(t *testing.T) {
		stt := newFakeSTT(t, "<html>proxy error</html>")
		b := newSTTFleet(stt.srv.URL)
		rec := httptest.NewRecorder()
		b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", strings.NewReader("audio")))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("want 502, got %d", rec.Code)
		}
	})
	t.Run("unreachable server", func(t *testing.T) {
		b := newSTTFleet("http://127.0.0.1:1/inference")
		rec := httptest.NewRecorder()
		b.handleSTT(rec, httptest.NewRequest("POST", "/api/stt", strings.NewReader("audio")))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("want 502, got %d", rec.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// /api/tts
// ---------------------------------------------------------------------------

func TestHandleTTS_Disabled(t *testing.T) {
	for _, b := range []*Server{{}, {conv: &convConfig{}}} {
		rec := httptest.NewRecorder()
		b.handleTTS(rec, httptest.NewRequest("POST", "/api/tts", strings.NewReader(`{"text":"hi"}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503 with no voice configured, got %d", rec.Code)
		}
	}
}

func TestHandleTTS_BadInput(t *testing.T) {
	b := &Server{conv: &convConfig{voice: "en-US-AriaNeural", client: &http.Client{}}}
	for _, body := range []string{`not json`, `{"text":""}`, `{"text":"   "}`, `{}`} {
		rec := httptest.NewRecorder()
		b.handleTTS(rec, httptest.NewRequest("POST", "/api/tts", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: want 400, got %d", body, rec.Code)
		}
	}
}

// When --tts-url is set it must win over the Edge websocket (Edge rides an
// unofficial endpoint Microsoft periodically re-keys). The upstream's own
// content type has to pass through — servers return mp3 or wav.
func TestHandleTTS_PrefersOpenAIEndpoint(t *testing.T) {
	var gotPayload map[string]any
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotPayload)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFFfake-wav-bytes"))
	}))
	defer tts.Close()

	b := &Server{conv: &convConfig{
		voice: "af_heart", ttsURL: tts.URL, ttsModel: "kokoro",
		client: &http.Client{Timeout: 30 * time.Second},
	}}
	rec := httptest.NewRecorder()
	b.handleTTS(rec, httptest.NewRequest("POST", "/api/tts", strings.NewReader(`{"text":"Aye, Admiral."}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "RIFFfake-wav-bytes" {
		t.Fatalf("audio not passed through: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("upstream content type must pass through, got %q", ct)
	}
	if rec.Header().Get("Content-Length") != "18" {
		t.Errorf("content length %q", rec.Header().Get("Content-Length"))
	}
	if gotPayload["input"] != "Aye, Admiral." || gotPayload["voice"] != "af_heart" ||
		gotPayload["model"] != "kokoro" || gotPayload["response_format"] != "mp3" {
		t.Errorf("request payload wrong: %+v", gotPayload)
	}
}

func TestHandleTTS_UpstreamErrorIs502(t *testing.T) {
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("voice not found"))
	}))
	defer tts.Close()

	b := &Server{conv: &convConfig{ttsURL: tts.URL, client: &http.Client{Timeout: 30 * time.Second}}}
	rec := httptest.NewRecorder()
	b.handleTTS(rec, httptest.NewRequest("POST", "/api/tts", strings.NewReader(`{"text":"hi"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "voice not found") {
		t.Errorf("upstream detail lost: %q", rec.Body.String())
	}
}

// A server that returns audio without a content type still has to be playable
// by <audio>, so we default it.
func TestSynthesizeOpenAITTS_DefaultsContentType(t *testing.T) {
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = w.Write([]byte("mp3"))
	}))
	defer tts.Close()

	b := &Server{conv: &convConfig{ttsURL: tts.URL, client: &http.Client{Timeout: 30 * time.Second}}}
	_, ctype, err := b.synthesizeOpenAITTS(t.Context(), "hi")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if ctype != "audio/mpeg" {
		t.Fatalf("want a default of audio/mpeg, got %q", ctype)
	}
}

// ---------------------------------------------------------------------------
// Edge TTS helpers (no network)
// ---------------------------------------------------------------------------

func TestEscapeSSML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"a & b", "a &amp; b"},
		{"<script>", "&lt;script&gt;"},
		{`say "hi"`, "say &quot;hi&quot;"},
		{"it's fine", "it&apos;s fine"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := escapeSSML(tc.in); got != tc.want {
			t.Errorf("escapeSSML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Ampersand must be escaped first or the entities double-escape.
	if got := escapeSSML("<a & b>"); strings.Contains(got, "&amp;lt;") {
		t.Errorf("double-escaped: %q", got)
	}
	// The escaped text must survive inside a real SSML document — one
	// unescaped character breaks the whole document and the request fails
	// wholesale, so round-trip it through an XML parser.
	hostile := `Tell <picard> that "A & B" isn't done — 5 > 3`
	doc := "<speak><voice name='x'>" + escapeSSML(hostile) + "</voice></speak>"
	var parsed struct {
		Voice string `xml:"voice"`
	}
	if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("escaped SSML does not parse: %v\n%s", err, doc)
	}
	if parsed.Voice != hostile {
		t.Errorf("SSML round trip changed the text:\n got %q\nwant %q", parsed.Voice, hostile)
	}
}

func TestComputeSecMSGEC(t *testing.T) {
	got := computeSecMSGEC()
	if len(got) != 64 {
		t.Fatalf("want a 64-char sha256 hex digest, got %d chars", len(got))
	}
	if got != strings.ToUpper(got) {
		t.Errorf("token must be uppercase: %q", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("token is not hex: %v", err)
	}
	// Time-bucketed to 5 minutes, so two calls in quick succession must match.
	if again := computeSecMSGEC(); again != got {
		t.Errorf("token is not stable within its 5-minute bucket: %q vs %q", got, again)
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGenUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		got := genUUID()
		if !uuidV4.MatchString(got) {
			t.Fatalf("not a v4 uuid: %q", got)
		}
		if seen[got] {
			t.Fatalf("duplicate uuid %q", got)
		}
		seen[got] = true
	}
}

func TestRandomHex(t *testing.T) {
	// Microsoft validates the MUID cookie's shape: 32 hex chars.
	got := randomHex(32)
	if len(got) != 32 {
		t.Fatalf("want 32 chars, got %d (%q)", len(got), got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Fatalf("not hex: %v", err)
	}
	if got == randomHex(32) {
		t.Errorf("MUID must be fresh per request")
	}
}
