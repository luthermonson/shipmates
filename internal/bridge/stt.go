package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// Speech-to-text: the browser records the operator's utterance as 16kHz mono
// WAV (Web Audio capture — chosen over MediaRecorder codecs so whisper.cpp
// can decode it without ffmpeg) and POSTs the bytes here. The bridge wraps
// them in the multipart form both whisper.cpp's /inference and any
// OpenAI-compatible /v1/audio/transcriptions server accept, and returns
// {"text": "..."} either way. This replaces the Web Speech API, whose
// unreliability on iOS is why voice mode was parked.

// sttMaxBytes bounds an utterance upload: 16kHz mono 16-bit is 32KB/s, so
// 4MB ≈ two minutes of speech — beyond any sane voice command.
const sttMaxBytes = 4 << 20

func (b *Server) handleSTT(w http.ResponseWriter, r *http.Request) {
	if b.conv == nil || b.conv.sttURL == "" {
		http.Error(w, "stt not configured (start the bridge with --stt-url)", http.StatusNotImplemented)
		return
	}
	audio, err := io.ReadAll(io.LimitReader(r.Body, sttMaxBytes+1))
	if err != nil || len(audio) == 0 {
		http.Error(w, "want audio bytes in the request body", http.StatusBadRequest)
		return
	}
	if len(audio) > sttMaxBytes {
		http.Error(w, "utterance too long", http.StatusRequestEntityTooLarge)
		return
	}

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, err := mw.CreateFormFile("file", "utterance.wav")
	if err == nil {
		_, err = fw.Write(audio)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// OAI-style servers require a model field; whisper.cpp ignores it.
	if b.conv.sttModel != "" {
		_ = mw.WriteField("model", b.conv.sttModel)
	}
	_ = mw.WriteField("response_format", "json")
	_ = mw.Close()

	req, err := http.NewRequestWithContext(r.Context(), "POST", b.conv.sttURL, &form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.conv.client.Do(req)
	if err != nil {
		http.Error(w, "stt server unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("stt server %d: %s", resp.StatusCode, string(body)), http.StatusBadGateway)
		return
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		http.Error(w, "stt server returned non-json: "+string(body[:min(len(body), 200)]), http.StatusBadGateway)
		return
	}
	// whisper.cpp embeds "\n " at its segment boundaries; speech has no line
	// structure, so collapse all whitespace runs to single spaces.
	text := strings.Join(strings.Fields(out.Text), " ")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
}

// handleVoiceConfig tells the conversation UI which voice capabilities this
// bridge was started with, so it can grey out the mic / skip TTS instead of
// failing on first use.
func (b *Server) handleVoiceConfig(w http.ResponseWriter, r *http.Request) {
	caps := map[string]bool{"conversation": false, "tts": false, "stt": false}
	if b.conv != nil {
		caps["conversation"] = b.conv.url != ""
		caps["tts"] = b.conv.voice != "" || b.conv.ttsURL != ""
		caps["stt"] = b.conv.sttURL != ""
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(caps)
}
