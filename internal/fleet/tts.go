package fleet

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Microsoft Edge browser's "Read Aloud" feature talks to a websocket service
// at speech.platform.bing.com. The auth model is a hard-coded Trusted Client
// Token plus a time-derived SHA256 token (Sec-MS-GEC) that Microsoft added in
// 2024 to deter unofficial use. Both are well-documented in the open-source
// edge-tts community. We use the same approach here so the fleet can speak
// with Microsoft's neural voices on mobile, where browser SpeechSynthesis is
// unreliable. If Microsoft ever rotates the token or hardens auth, swap to
// Piper (offline) or the official Azure Speech Service.

const (
	edgeTTSURL          = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	edgeTTSTrustedToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	// Pinned to the latest Edge release that the edge-tts community library is
	// known-working against. Microsoft tightens this validation occasionally;
	// when /api/tts starts 403'ing, bump these to match whatever current
	// `rany2/edge-tts` ships with.
	edgeTTSGECVersion = "1-143.0.3650.75"
	edgeTTSOrigin     = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"
	// "audio-24khz-48kbitrate-mono-mp3" is the format Edge browser uses for
	// Read Aloud — universally playable via HTML5 <audio>, ~6KB/sec mono.
	edgeTTSAudioFormat = "audio-24khz-48kbitrate-mono-mp3"
	edgeTTSDefaultUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
)

// synthesizeEdgeTTS turns text into MP3 bytes via Microsoft's Edge Read-Aloud
// websocket. voice is e.g. "en-US-AriaNeural". Returns the full MP3 stream once
// the service signals turn.end.
func synthesizeEdgeTTS(ctx context.Context, text, voice string) ([]byte, error) {
	if voice == "" {
		voice = "en-US-AriaNeural"
	}
	u := fmt.Sprintf("%s?TrustedClientToken=%s&Sec-MS-GEC=%s&Sec-MS-GEC-Version=%s",
		edgeTTSURL, edgeTTSTrustedToken, computeSecMSGEC(), edgeTTSGECVersion)

	headers := http.Header{}
	headers.Set("Origin", edgeTTSOrigin)
	headers.Set("User-Agent", edgeTTSDefaultUA)
	// A random Machine-User-ID cookie is part of what Microsoft validates now;
	// any 32-hex-char value works as long as it's present. Fresh per request.
	headers.Set("Cookie", "MUID="+randomHex(32))

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, resp, err := dialer.DialContext(dctx, u, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("edge tts dial: %s (%w)", resp.Status, err)
		}
		return nil, fmt.Errorf("edge tts dial: %w", err)
	}
	defer conn.Close()

	// Config message tells the service what audio format to return and disables
	// per-word metadata we don't use.
	configBody, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"synthesis": map[string]any{
				"audio": map[string]any{
					"metadataoptions": map[string]any{
						"sentenceBoundaryEnabled": false,
						"wordBoundaryEnabled":     false,
					},
					"outputFormat": edgeTTSAudioFormat,
				},
			},
		},
	})
	requestID := strings.ReplaceAll(genUUID(), "-", "")
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	configMsg := fmt.Sprintf(
		"X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n%s",
		ts, string(configBody),
	)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(configMsg)); err != nil {
		return nil, fmt.Errorf("send config: %w", err)
	}

	// SSML message: the actual text to speak, wrapped in <voice><prosody>.
	ssml := fmt.Sprintf(
		`<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'>`+
			`<voice name='%s'><prosody pitch='+0Hz' rate='+0%%' volume='+0%%'>%s</prosody></voice></speak>`,
		voice, escapeSSML(text),
	)
	ssmlMsg := fmt.Sprintf(
		"X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%s\r\nPath:ssml\r\n\r\n%s",
		requestID, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), ssml,
	)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(ssmlMsg)); err != nil {
		return nil, fmt.Errorf("send ssml: %w", err)
	}

	var audio bytes.Buffer
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		select {
		case <-dctx.Done():
			return nil, dctx.Err()
		default:
		}
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		switch mt {
		case websocket.TextMessage:
			// Text messages signal stream state. The one we care about is
			// "Path:turn.end" — the audio for this request is fully delivered.
			if strings.Contains(string(msg), "Path:turn.end") {
				return audio.Bytes(), nil
			}
		case websocket.BinaryMessage:
			// Binary messages carry an HTTP-like header section followed by
			// raw audio bytes. The first 2 bytes are the header length (BE).
			if len(msg) < 2 {
				continue
			}
			headerLen := binary.BigEndian.Uint16(msg[:2])
			if int(headerLen)+2 > len(msg) {
				continue
			}
			audio.Write(msg[2+headerLen:])
		}
	}
}

// computeSecMSGEC builds the time-derived auth token Microsoft added in 2024.
// Algorithm reverse-engineered from Edge browser and edge-tts library:
//
//	ticks = windows-file-time(now) rounded down to 5-minute boundary
//	token = uppercase(sha256(ticks + TrustedClientToken))
//
// Time-bucketed so the same value is valid for 5 minutes — a slight clock skew
// from this box vs Microsoft's edge is fine.
func computeSecMSGEC() string {
	// Windows file time: 100-ns ticks since 1601-01-01 UTC.
	// Difference from Unix epoch is 11644473600 seconds.
	ticks := (time.Now().Unix() + 11644473600) * 10_000_000
	ticks -= ticks % 3_000_000_000 // round down to nearest 5 minutes
	text := fmt.Sprintf("%d%s", ticks, edgeTTSTrustedToken)
	sum := sha256.Sum256([]byte(text))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// escapeSSML escapes the few characters that have meaning inside SSML text:
// & < > " '. Anything else passes through verbatim.
func escapeSSML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// genUUID is a thin wrapper so this file is self-contained. We can't import
// project.NewUUID here (cyclic via server→project→...→fleet); reproduce the
// same v4 logic.
func genUUID() string {
	var b [16]byte
	_, _ = randRead(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randRead is split out so callers can fail closed on /dev/urandom failure.
func randRead(b []byte) (int, error) {
	return cryptoRand.Read(b)
}

// randomHex returns n hex characters (n/2 random bytes). Used for the MUID
// cookie Microsoft now expects on the Edge TTS websocket handshake.
func randomHex(n int) string {
	b := make([]byte, n/2)
	_, _ = cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

// handleTTS is the fleet's /api/tts endpoint. The conversation UI POSTs the
// reply text after every turn and gets back MP3 bytes to play via <audio>.
// We use POST not GET because long replies can exceed URL length limits, and
// because TTS is conceptually a synthesis operation, not a fetch.
func (b *Server) handleTTS(w http.ResponseWriter, r *http.Request) {
	if b.conv == nil || (b.conv.voice == "" && b.conv.ttsURL == "") {
		http.Error(w, "tts disabled — fleet serve --tts-voice or --tts-url required", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "empty text", http.StatusBadRequest)
		return
	}
	var audio []byte
	ctype := "audio/mpeg"
	var err error
	if b.conv.ttsURL != "" {
		// local/model path: any OpenAI-compatible /v1/audio/speech server
		// (kokoro-fastapi, speaches, openedai-speech, LocalAI, or OpenAI
		// itself) — preferred over Edge when configured, since Edge rides an
		// unofficial endpoint Microsoft occasionally re-keys.
		audio, ctype, err = b.synthesizeOpenAITTS(r.Context(), req.Text)
	} else {
		audio, err = synthesizeEdgeTTS(r.Context(), req.Text, b.conv.voice)
	}
	if err != nil {
		http.Error(w, "tts: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(audio)))
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(audio)
}

// synthesizeOpenAITTS posts to an OpenAI-compatible speech endpoint and
// returns the audio bytes plus their content type (servers vary: mp3 or wav —
// HTML5 <audio> plays both, so pass whatever came back through). Voice and
// model ride the convConfig (--tts-voice doubles as the voice name for these
// servers, e.g. kokoro's "af_heart").
func (b *Server) synthesizeOpenAITTS(ctx context.Context, text string) ([]byte, string, error) {
	payload := map[string]any{
		"input":           text,
		"response_format": "mp3",
	}
	if b.conv.ttsModel != "" {
		payload["model"] = b.conv.ttsModel
	}
	if b.conv.voice != "" {
		payload["voice"] = b.conv.voice
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", b.conv.ttsURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.conv.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	audio, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("tts server %d: %s", resp.StatusCode, string(audio[:min(len(audio), 200)]))
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "audio/mpeg"
	}
	return audio, ctype, nil
}
