// Voice + text conversation with the bridge's captain's mate.
//
// Mic path v2: Web Audio capture → 16kHz mono WAV → POST /api/stt → whisper
// on the bridge side. The Web Speech API this replaced was the reason voice
// mode got parked — SpeechRecognition is unreliable-to-absent on iOS, while
// getUserMedia + AudioContext work everywhere. WAV (not MediaRecorder's
// aac/opus containers) so whisper.cpp decodes it without ffmpeg.
// Replies come back from POST /api/conversation and are spoken via /api/tts.

const convo = document.getElementById("convo");
const mic = document.getElementById("mic");
const input = document.getElementById("text-input");
const send = document.getElementById("send");
const hint = document.getElementById("hint");

let history = []; // chat history kept in memory for this session only

// capability probe: grey out what the bridge wasn't started with
let caps = { conversation: true, tts: true, stt: true };
fetch("/api/voice/config").then((r) => r.ok ? r.json() : caps).then((c) => {
  caps = c;
  if (!caps.stt) {
    mic.disabled = true;
    mic.title = "bridge started without --stt-url";
    hint.textContent = "voice input not configured on the bridge (--stt-url) — type your messages.";
  }
  if (!caps.conversation) {
    hint.textContent = "conversation loop not configured on the bridge (--ollama-url).";
    send.disabled = true;
  }
}).catch(() => {});

// --- microphone capture + conversation mode -----------------------------------
//
// One tap turns the conversation ON: the mic stays open and a small energy
// VAD (voice activity detector) segments your speech — roughly a second of
// silence after you talk auto-cuts the utterance, transcribes it, asks the
// fleet, speaks the answer, and goes back to listening. Tap again to end.
// Capture pauses while the reply plays so the assistant doesn't hear itself.
//
// ScriptProcessorNode is deprecated but is the one capture API that works
// everywhere including iOS Safari.

let rec = null;        // {ctx, stream, src, node, gain, rate}
let live = false;      // conversation mode on
let vadState = "idle"; // idle | listening | speaking | busy

const VAD_SILENCE_MS = 1100; // this much quiet ends the utterance
const VAD_MIN_SPEECH_MS = 350; // shorter blips (coughs, taps) are ignored
const VAD_MAX_UTTER_MS = 60000; // hard cap per utterance

let noiseFloor = 0.006; // adapts while listening
let preRoll = [];       // recent chunks so the utterance start isn't clipped
let speechBufs = [];    // the utterance being captured
let speechMs = 0, silentMs = 0, utterMs = 0;

async function startCapture() {
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true },
    });
  } catch (err) {
    hint.innerHTML = "mic blocked — tap the padlock in the address bar → site settings → microphone → allow, then reload. " +
                     "(on ios chrome you may need Settings → Chrome → Microphone)";
    return false;
  }
  const ctx = new (window.AudioContext || window.webkitAudioContext)();
  await ctx.resume(); // iOS: must resume inside the user gesture
  const src = ctx.createMediaStreamSource(stream);
  const node = ctx.createScriptProcessor(4096, 1, 1);
  node.onaudioprocess = (e) => onAudio(new Float32Array(e.inputBuffer.getChannelData(0)));
  // the processor only runs when connected to the destination; a zero-gain
  // stage keeps the mic from feeding back through the speakers
  const gain = ctx.createGain();
  gain.gain.value = 0;
  src.connect(node); node.connect(gain); gain.connect(ctx.destination);
  rec = { ctx, stream, src, node, gain, rate: ctx.sampleRate };
  return true;
}

function stopCapture() {
  if (!rec) return;
  const { ctx, stream, src, node, gain } = rec;
  rec = null;
  try { src.disconnect(); node.disconnect(); gain.disconnect(); } catch { /* already torn down */ }
  stream.getTracks().forEach((t) => t.stop());
  ctx.close().catch(() => {});
  preRoll = []; speechBufs = [];
}

// onAudio is the VAD state machine, fed ~85ms buffers while the mic is open.
function onAudio(buf) {
  if (!rec) return;
  const ms = (buf.length / rec.rate) * 1000;
  let sum = 0;
  for (let i = 0; i < buf.length; i++) sum += buf[i] * buf[i];
  const rms = Math.sqrt(sum / buf.length);
  const threshold = Math.max(noiseFloor * 3, 0.012);

  if (vadState === "listening") {
    noiseFloor = noiseFloor * 0.95 + rms * 0.05; // track the room
    preRoll.push(buf);
    if (preRoll.length > 8) preRoll.shift(); // ~0.7s of lead-in
    if (rms > threshold) {
      vadState = "speaking";
      speechBufs = preRoll.slice();
      preRoll = [];
      speechMs = ms; silentMs = 0; utterMs = ms;
      mic.classList.add("recording");
      hint.textContent = "";
    }
  } else if (vadState === "speaking") {
    speechBufs.push(buf);
    utterMs += ms;
    if (rms > Math.max(noiseFloor * 2.5, 0.01)) { speechMs += ms; silentMs = 0; }
    else silentMs += ms;
    if (silentMs >= VAD_SILENCE_MS || utterMs >= VAD_MAX_UTTER_MS) endUtterance();
  }
  // idle/busy: the reply pipeline owns the floor; audio is discarded
}

async function endUtterance() {
  vadState = "busy";
  mic.classList.remove("recording");
  const bufs = speechBufs;
  speechBufs = [];
  if (speechMs < VAD_MIN_SPEECH_MS) { resumeListening(); return; } // a blip, not speech
  let n = 0;
  for (const c of bufs) n += c.length;
  const all = new Float32Array(n);
  let off = 0;
  for (const c of bufs) { all.set(c, off); off += c.length; }
  const wav = encodeWAV(downsampleTo16k(all, rec ? rec.rate : 48000));
  const text = await transcribe(wav);
  if (!text) { resumeListening(); return; }
  input.value = text;
  await sendMessage();
  // if a spoken reply is playing, its 'ended' event resumes listening;
  // otherwise (tts off / failed / empty reply) resume now
  if (audioEl.paused || audioEl.ended) resumeListening();
}

function resumeListening() {
  if (!live) return;
  vadState = "listening";
  silentMs = 0; speechMs = 0; utterMs = 0;
  hint.textContent = "listening — just talk; tap the mic to end";
}

// downsampleTo16k averages source samples per output sample — crude but fine
// for speech, and whisper wants 16kHz anyway.
function downsampleTo16k(samples, fromRate) {
  const outRate = 16000;
  if (fromRate === outRate) return samples;
  const ratio = fromRate / outRate;
  const out = new Float32Array(Math.floor(samples.length / ratio));
  for (let i = 0; i < out.length; i++) {
    const start = Math.floor(i * ratio), end = Math.min(Math.floor((i + 1) * ratio), samples.length);
    let sum = 0;
    for (let j = start; j < end; j++) sum += samples[j];
    out[i] = sum / Math.max(1, end - start);
  }
  return out;
}

// encodeWAV wraps float samples as 16-bit PCM mono RIFF at 16kHz.
function encodeWAV(samples) {
  const buf = new ArrayBuffer(44 + samples.length * 2);
  const v = new DataView(buf);
  const wstr = (o, s) => { for (let i = 0; i < s.length; i++) v.setUint8(o + i, s.charCodeAt(i)); };
  wstr(0, "RIFF"); v.setUint32(4, 36 + samples.length * 2, true); wstr(8, "WAVE");
  wstr(12, "fmt "); v.setUint32(16, 16, true); v.setUint16(20, 1, true); v.setUint16(22, 1, true);
  v.setUint32(24, 16000, true); v.setUint32(28, 16000 * 2, true); v.setUint16(32, 2, true); v.setUint16(34, 16, true);
  wstr(36, "data"); v.setUint32(40, samples.length * 2, true);
  for (let i = 0; i < samples.length; i++) {
    const s = Math.max(-1, Math.min(1, samples[i]));
    v.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  return new Blob([buf], { type: "audio/wav" });
}

async function transcribe(wav) {
  hint.textContent = "transcribing…";
  try {
    const r = await fetch("/api/stt", { method: "POST", headers: { "Content-Type": "audio/wav" }, body: wav });
    if (r.status === 401) { window.location.href = "/login"; return ""; }
    if (!r.ok) { hint.textContent = "stt: " + (await r.text()).slice(0, 160); return ""; }
    hint.textContent = "";
    return (await r.json()).text || "";
  } catch (err) {
    hint.textContent = "stt unreachable: " + String(err).slice(0, 120);
    return "";
  }
}

// iOS Safari requires the FIRST .play() on an Audio element to fire inside a
// user gesture handler. Replies arrive seconds later in an async callback, by
// which point the gesture no longer counts. Fix: call .play() with a tiny
// silent src during the mic-click gesture so the element is "unlocked" — all
// subsequent .play() calls within the same page lifetime then work.
const silentMP3 = "data:audio/mpeg;base64,SUQzBAAAAAAAI1RTU0UAAAAPAAADTGF2ZjU4Ljc2LjEwMAAAAAAAAAAAAAAA//tQwAAAAAAAAAAAAAAAAAAAAAAASW5mbwAAAA8AAAACAAABIADAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMD//////////////////////////////////////////8AAAAATGF2YzU4LjEzAAAAAAAAAAAAAAAAJAAAAAAAAAAAASDs90hvAAAAAAAAAAAAAAAAAAAA//tQxAADwAABpAAAACAAADSAAAAETEFNRTMuMTAwVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVV//tQxFKDwAABpAAAACAAADSAAAAEVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVV//tQxKqDwAABpAAAACAAADSAAAAEVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVVV//tQxAOAA"; // tiny inaudible MP3 frame
let audioUnlocked = false;
function unlockTTS() {
  if (audioUnlocked) return;
  audioEl.src = silentMP3;
  audioEl.play().catch(() => { /* iOS may reject the very first one; future calls work */ });
  audioUnlocked = true;
}

// tap once = conversation ON (hands-free, VAD-segmented); tap again = off
mic.onclick = async () => {
  if (mic.disabled) return;
  unlockTTS();
  if (live) {
    live = false;
    vadState = "idle";
    stopCapture();
    mic.classList.remove("live", "recording");
    hint.textContent = "";
    return;
  }
  if (!(await startCapture())) return;
  live = true;
  mic.classList.add("live");
  resumeListening();
};

// Also unlock on send-button / Enter, for the text-input path.
send.addEventListener("click", unlockTTS);
input.addEventListener("keydown", (e) => { if (e.key === "Enter") unlockTTS(); });

send.onclick = sendMessage;
input.addEventListener("keydown", (e) => {
  if (e.key === "Enter") sendMessage();
});

async function sendMessage() {
  const text = input.value.trim();
  if (!text) return;
  input.value = "";
  addBubble("user", text);
  history.push({ role: "user", content: text });
  const thinking = addBubble("thinking", "thinking…");
  send.disabled = true; mic.disabled = true;
  try {
    const r = await fetch("/api/conversation", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ messages: history }),
    });
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) {
      const err = await r.text();
      thinking.remove();
      addBubble("assistant", "error: " + err);
      return;
    }
    const data = await r.json();
    thinking.remove();
    for (const tc of (data.tools_called || [])) {
      addBubble("tool", `→ ${tc.function.name}(${formatArgs(tc.function.arguments)})`);
    }
    addBubble("assistant", data.reply || "(empty reply)");
    history.push({ role: "assistant", content: data.reply || "" });
    speak(data.reply || "");
  } catch (err) {
    thinking.remove();
    addBubble("assistant", "network error: " + String(err));
  } finally {
    send.disabled = false; mic.disabled = !caps.stt;
    input.focus();
  }
}

function addBubble(role, text) {
  const div = document.createElement("div");
  div.className = "bubble " + role;
  div.textContent = text;
  convo.appendChild(div);
  convo.scrollTop = convo.scrollHeight;
  return div;
}

function formatArgs(args) {
  // OpenAI wire format: arguments arrive as a JSON-encoded string
  if (typeof args === "string") {
    try { args = JSON.parse(args); } catch { return args.slice(0, 80); }
  }
  if (!args || Object.keys(args).length === 0) return "";
  return Object.entries(args).map(([k, v]) => `${k}=${JSON.stringify(v)}`).join(", ");
}

// Server-side TTS: replies are POSTed to /api/tts (Edge's neural voices via
// the read-aloud websocket), and the MP3 audio comes back as a blob we play
// through an HTML5 <audio> element. This bypasses SpeechSynthesis entirely —
// iOS's spotty SpeechSynthesis implementation was the reason TTS often went
// mute on phones, and HTML5 audio is rock-solid across platforms.
const audioEl = new Audio();
audioEl.preload = "auto";
const ttsTest = document.getElementById("tts-test");
const ttsInfo = document.getElementById("tts-info");
ttsInfo.textContent = "server-side TTS (Edge neural)";

async function speak(text) {
  if (!text) return;
  try {
    const r = await fetch("/api/tts", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text }),
    });
    if (!r.ok) {
      ttsInfo.textContent = `tts ${r.status}: ${await r.text()}`;
      return;
    }
    const blob = await r.blob();
    audioEl.src = URL.createObjectURL(blob);
    await audioEl.play();
  } catch (err) {
    ttsInfo.textContent = "tts: " + String(err);
  }
}

// conversation mode: the mic stays muted while the reply plays (vadState
// "busy"); when playback finishes, go back to listening
audioEl.addEventListener("ended", resumeListening);
audioEl.addEventListener("error", resumeListening);

// iOS suspends the AudioContext when the tab backgrounds; wake it (and the
// listening loop) when the operator comes back mid-conversation.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && live && rec) {
    rec.ctx.resume().catch(() => {});
    if (vadState === "idle") resumeListening();
  }
});

// Diagnostic button — fires fetch+play directly on click (inside the user
// gesture, which keeps iOS Safari happy on first use).
ttsTest.onclick = async () => {
  ttsInfo.textContent = "fetching…";
  await speak("Testing the bridge. If you hear this, voice output works.");
  ttsInfo.textContent = "test fired";
};
