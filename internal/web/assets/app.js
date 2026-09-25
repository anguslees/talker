import { PCMPlayer } from "./audio.js";
import { DirectLive } from "./live.js";

const $ = id => document.getElementById(id);
const ui = Object.fromEntries([
  "voice-console", "voice-title", "voice-description", "voice-kicker", "connection-label",
  "start-button", "mute-button", "stop-button", "mic-dot", "mic-label", "session-clock",
  "setting-model", "setting-voice", "setting-input", "status-message", "error-message",
  "transcript", "transcript-empty", "text-input", "send-button", "notifications",
  "notification-count", "tasks", "task-count",
].map(id => [id, $(id)]));

const MAX_TRANSCRIPT_ENTRIES = 80;
const MAX_TRANSCRIPT_TEXT = 16000;
const MAX_NOTIFICATIONS = 30;
const MAX_TASKS = 50;
const notifications = new Map();
const dismissed = new Set();
const drafts = new Map();
let session = null;
let taskRevision = 0;
let notificationRevision = 0;
let snapshotPending = false;

const clip = (value, length = 4000) => typeof value === "string" ? value.slice(0, length) : "";

function message(kind, text) {
  const element = ui[`${kind}-message`];
  element.textContent = clip(text);
  element.hidden = !text;
}

function renderState() {
  const s = session;
  const connected = s?.phase === "connected";
  let state = "idle";
  let title = "Room to think out loud.";
  let description = "Talk it through. Let the rest run in the background.";
  let kicker = "A LITTLE LESS TYPING.";
  let connection = "Offline";
  if (s) {
    state = "connecting";
    title = s.phase === "reconnecting" ? "Finding the thread again." : "Opening your space.";
    description = s.phase === "reconnecting" ? "Microphone paused. It resumes when Gemini is ready." : "Connecting audio and checking microphone access.";
    kicker = s.phase === "reconnecting" ? "RECONNECTING / MIC PAUSED" : "ONE MOMENT";
    connection = s.phase === "reconnecting" ? "Reconnecting" : "Connecting";
    if (connected) {
      connection = "Live / direct";
      state = s.player.active ? "speaking" : s.muted ? "muted" : s.speaking ? "hearing" : s.thinking ? "thinking" : "listening";
      const labels = {
        speaking: ["A thought, taking shape.", "Talker is speaking. You can speak naturally to respond.", "TALKER / SPEAKING"],
        muted: ["A moment of quiet.", "Your microphone is muted. You can still listen or type.", "MICROPHONE / MUTED"],
        hearing: ["Go on. I'm listening.", "There is room for the whole thought.", "YOU / SPEAKING"],
        thinking: ["Putting it together.", "Your thought is in good company.", "TALKER / THINKING"],
        listening: ["What's on your mind?", "Speak naturally. No need to hold a button.", "YOUR SPACE / LISTENING"],
      };
      [title, description, kicker] = labels[state];
    }
  }
  ui["voice-console"].dataset.state = state;
  ui["voice-title"].textContent = title;
  ui["voice-description"].textContent = description;
  ui["voice-kicker"].textContent = kicker;
  ui["connection-label"].textContent = connection;
  ui["start-button"].disabled = !!s;
  ui["stop-button"].disabled = !s;
  ui["mute-button"].disabled = !connected;
  ui["mute-button"].setAttribute("aria-pressed", String(!!s?.muted));
  ui["mute-button"].querySelector("span").textContent = s?.muted ? "Unmute mic" : "Mute mic";
  ui["mic-label"].textContent = connected ? (s.muted ? "Microphone muted" : "Microphone on") : s ? "Microphone paused" : "Microphone off";
  ui["mic-dot"].classList.toggle("active", connected && !s.muted);
  ui["text-input"].disabled = !connected;
  ui["send-button"].disabled = !connected;
  for (const button of ui.notifications.querySelectorAll("button")) {
    const pending = s?.live?.ackPending.has(button.dataset.id);
    button.disabled = !s?.live?.controlReady || pending;
    button.textContent = pending ? "Confirming..." : "Mark read";
    button.title = s?.live?.controlReady ? "Mark this update as read" : "Start a session to mark this update as read";
  }
}

function friendlyError(error) {
  switch (error?.name) {
    case "NotAllowedError": return "Microphone access was not granted. Allow the microphone in your browser's site settings, then start again.";
    case "NotFoundError": return "No microphone was found. Connect a microphone, then start again.";
    case "NotReadableError": return "Your microphone could not be opened. Check that another application is not holding it, then start again.";
    case "OverconstrainedError": return "This microphone cannot provide the requested audio. Select another input in your browser and start again.";
    case "SecurityError": return "Microphone access is blocked. Open Talker on localhost or HTTPS and allow microphone access.";
    default: return clip(error?.message) || "The audio session could not start. Check microphone access and try again.";
  }
}

function setCapture(s, enabled) {
  s.inputEpoch++;
  const wasSpeaking = s.speaking;
  s.speaking = false;
  for (const track of s.stream?.getAudioTracks() || []) track.enabled = enabled;
  s.mic?.port.postMessage({ type: "configure", enabled, epoch: s.inputEpoch });
  s.live?.setInputReady(!!s.stream && s.phase === "connected");
  if (wasSpeaking) s.live?.setUserActivity(false);
}

function finishDrafts() {
  for (const entry of drafts.values()) entry.dataset.pending = "false";
  drafts.clear();
}

function closeSession(s, text) {
  if (session !== s) return;
  session = null;
  clearTimeout(s.timeout);
  clearInterval(s.watchdog);
  s.live?.close();
  for (const track of s.stream?.getTracks() || []) {
    track.onended = null;
    track.stop();
  }
  if (s.mic) {
    s.mic.port.onmessage = null;
    s.mic.port.postMessage({ type: "configure", enabled: false, epoch: ++s.inputEpoch });
    s.mic.port.close();
    s.mic.disconnect();
  }
  s.source?.disconnect();
  s.silent?.disconnect();
  s.player?.close();
  if (s.context) {
    s.context.onstatechange = null;
    void s.context.close().catch(() => {});
  }
  if (s.wakeLock) void s.wakeLock.release().catch(() => {});
  finishDrafts();
  ui["setting-input"].textContent = "Not active";
  renderState();
  if (text) message("status", text);
  void refreshSnapshots();
}

function fail(s, error) {
  if (session !== s) return;
  closeSession(s, "Voice session ended. Background tasks keep running. Start again when you are ready.");
  message("error", friendlyError(error));
}

async function requestWakeLock(s) {
  if (!navigator.wakeLock || document.visibilityState !== "visible" || s.wakeLock || s.wakePending) return;
  s.wakePending = true;
  try {
    const lock = await navigator.wakeLock.request("screen");
    if (session !== s) {
      await lock.release();
      return;
    }
    s.wakeLock = lock;
    lock.addEventListener("release", () => { if (s.wakeLock === lock) s.wakeLock = null; });
  } catch { /* Wake lock is optional and may be denied by browser policy. */ }
  finally { s.wakePending = false; }
}

function configureReady(s, data) {
  ui["setting-model"].textContent = clip(data.model, 200) || "Server default";
  ui["setting-voice"].textContent = clip(data.voice, 100) || "Server default";
  s.ready = true;
  if (s.phase === "reconnecting" && s.mic) {
    clearTimeout(s.timeout);
    s.phase = "connected";
    setCapture(s, !s.muted);
    s.live.setMuted(s.muted);
    message("status", "Direct Google connection resumed. Your voice session is live again.");
    renderState();
  }
}

function createTransport(s) {
  const current = callback => value => { if (session === s) callback(value); };
  return new DirectLive({
    onReady: current(data => configureReady(s, data)),
    onPhase: current(phase => {
      s.ready = false;
      s.phase = phase;
      setCapture(s, false);
      finishDrafts();
      message("status", "Resuming the direct Google connection. Microphone input is paused; nothing is buffered or replayed.");
      renderState();
    }),
    onAudio: current(pcm => {
      try { s.player.push(pcm); } catch (error) { fail(s, error); }
    }),
    onInterrupted: current(() => {
      s.player.flush();
      s.thinking = false;
      if (drafts.has("assistant")) {
        drafts.get("assistant").dataset.pending = "false";
        drafts.delete("assistant");
      }
      renderState();
    }),
    onTranscript: current(appendTranscript),
    onTurnComplete: current(() => { finishDrafts(); renderState(); }),
    onGenerating: current(active => { s.thinking = active; renderState(); }),
    onTasks: current(renderTasks),
    onEvents: current(renderEventSnapshot),
    onAcknowledged: current(ids => {
      for (const id of ids) dismissed.add(id);
      while (dismissed.size > 200) dismissed.delete(dismissed.values().next().value);
    }),
    onStatus: current(text => message("status", text)),
    onError: current(error => fail(s, error)),
  });
}

async function startSession() {
  if (session) return;
  message("error", "");
  message("status", "");
  if (!window.isSecureContext) {
    message("error", "Microphone access requires HTTPS or localhost. Open Talker through an HTTPS proxy or a localhost SSH tunnel.");
    return;
  }
  const AudioContext = window.AudioContext || window.webkitAudioContext;
  if (!AudioContext || !window.AudioWorkletNode || !navigator.mediaDevices?.getUserMedia || !window.WebSocket) {
    message("error", "This browser does not support the audio features Talker needs. Use a current Chrome, Edge, Firefox, or Safari browser on localhost or HTTPS.");
    return;
  }
  const s = { phase: "connecting", ready: false, muted: false, speaking: false, thinking: false, inputEpoch: 0 };
  session = s;
  renderState();
  try {
    s.context = new AudioContext({ latencyHint: "interactive" });
    // Resume must run in the Start button's gesture, before any network await.
    const resumed = s.context.resume();
    s.timeout = setTimeout(() => fail(s, new Error("Starting the session timed out. Check the connection and microphone permission, then try again.")), 30000);
    await resumed;
    if (session !== s) return;
    await s.context.audioWorklet.addModule("/mic-worklet.js");
    if (session !== s) return;
    await s.context.audioWorklet.addModule("/playback-worklet.js");
    if (session !== s) return;
    s.player = new PCMPlayer(s.context, active => {
      if (session !== s) return;
      s.live?.setPlayback(active);
      renderState();
    });
    s.player.onError = error => fail(s, error);
    s.live = createTransport(s);
    await s.live.start();
    if (session !== s || !s.live.ready) return;
    const stream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: { ideal: 1 }, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      video: false,
    });
    if (session !== s || !s.live.ready || !s.live.controlReady) {
      for (const track of stream.getTracks()) track.stop();
      if (session === s) fail(s, new Error("The connection changed while opening the microphone. Start again."));
      return;
    }
    s.stream = stream;
    if (s.context.state !== "running") throw new Error("Browser audio was suspended while connecting. Start a new session.");
    s.source = s.context.createMediaStreamSource(stream);
    s.mic = new AudioWorkletNode(s.context, "talker-microphone", { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] });
    s.mic.onprocessorerror = () => fail(s, new Error("Microphone processing stopped. Start a new session."));
    s.silent = s.context.createGain();
    s.silent.gain.value = 0;
    s.mic.port.onmessage = ({ data }) => {
      if (session !== s || data.epoch !== s.inputEpoch || s.phase !== "connected" || s.muted) return;
      if (data.type === "overflow") {
        fail(s, new Error("Microphone processing fell behind. The session stopped rather than sending delayed audio."));
        return;
      }
      if (data.type !== "frame") return;
      try {
        if (s.speaking !== data.speaking) {
          s.speaking = data.speaking;
          s.live.setUserActivity(s.speaking);
          renderState();
        }
        if (!s.live.sendPCM(data.pcm)) return;
        s.mic.port.postMessage({ type: "credit", epoch: s.inputEpoch });
      } catch (error) { fail(s, error); }
    };
    s.source.connect(s.mic);
    s.mic.connect(s.silent);
    s.silent.connect(s.context.destination);
    s.phase = "connected";
    s.started = Date.now();
    s.lastWall = performance.now();
    s.lastAudio = s.context.currentTime;
    setCapture(s, true);
    s.live.setMuted(false);
    for (const track of stream.getTracks()) track.onended = () => fail(s, new Error("Your microphone disconnected or access was revoked. Start again after checking the input."));
    s.context.onstatechange = () => {
      if (session === s && s.context.state !== "running") fail(s, new Error("Browser audio was suspended. Return to this page and start a new session."));
    };
    s.watchdog = setInterval(() => {
      if (session !== s) return;
      const wall = performance.now();
      const audio = s.context.currentTime;
      if (wall - s.lastWall - (audio - s.lastAudio) * 1000 > 2000) {
        fail(s, new Error("Browser audio stalled. Start a new session to continue without delayed audio."));
        return;
      }
      s.lastWall = wall;
      s.lastAudio = audio;
      const seconds = Math.floor((Date.now() - s.started) / 1000);
      ui["session-clock"].textContent = `${String(Math.floor(seconds / 60)).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`;
    }, 500);
    clearTimeout(s.timeout);
    ui["session-clock"].textContent = "00:00";
    ui["setting-input"].textContent = `${s.context.sampleRate.toLocaleString()} Hz device / 16,000 Hz PCM`;
    renderState();
    void requestWakeLock(s);
  } catch (error) {
    fail(s, error);
  }
}

function appendTranscript(data) {
  if (data.role !== "user" && data.role !== "assistant") return;
  const text = clip(data.text, MAX_TRANSCRIPT_TEXT);
  let entry = drafts.get(data.role);
  if (!text && !entry) return;
  const follow = ui.transcript.scrollHeight - ui.transcript.scrollTop - ui.transcript.clientHeight < 80;
  if (!entry) {
    ui["transcript-empty"].hidden = true;
    entry = document.createElement("article");
    entry.className = "transcript-entry";
    entry.dataset.role = data.role;
    const meta = document.createElement("div");
    meta.className = "transcript-meta";
    const speaker = document.createElement("span");
    speaker.className = "transcript-speaker";
    speaker.textContent = data.role === "user" ? "You" : "Talker";
    const time = document.createElement("time");
    time.dateTime = new Date().toISOString();
    time.textContent = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    meta.append(speaker, time);
    entry.append(meta, document.createElement("p"));
    ui.transcript.append(entry);
    drafts.set(data.role, entry);
  }
  const content = entry.querySelector("p");
  const combined = content.textContent + text;
  content.textContent = combined.length > MAX_TRANSCRIPT_TEXT ? `[Earlier text omitted]\n${combined.slice(-MAX_TRANSCRIPT_TEXT)}` : combined;
  entry.dataset.pending = String(!data.finished);
  if (data.finished) drafts.delete(data.role);
  const entries = ui.transcript.querySelectorAll(".transcript-entry");
  for (let i = 0; i < entries.length - MAX_TRANSCRIPT_ENTRIES; i++) {
    if (drafts.get(entries[i].dataset.role) === entries[i]) drafts.delete(entries[i].dataset.role);
    entries[i].remove();
  }
  if (follow) ui.transcript.scrollTop = ui.transcript.scrollHeight;
}

function renderEventSnapshot(events) {
  if (!Array.isArray(events)) return;
  notifications.clear();
  for (const data of events.slice(-MAX_NOTIFICATIONS)) {
    if (!data || typeof data.id !== "string" || !data.id || data.id.length > 256 || dismissed.has(data.id)) continue;
    notifications.set(data.id, { id: data.id, title: clip(data.title, 200), body: clip(data.body, 16000) });
  }
  notificationRevision++;
  renderNotifications();
}

function renderNotifications() {
  ui.notifications.replaceChildren();
  ui["notification-count"].textContent = String(notifications.size);
  if (!notifications.size) {
    const empty = document.createElement("p");
    empty.className = "sidebar-empty";
    empty.textContent = "A quiet place for things worth knowing.";
    ui.notifications.append(empty);
  }
  for (const note of [...notifications.values()].reverse()) {
    const card = document.createElement("article");
    card.className = "notification-card";
    const title = document.createElement("h4");
    title.textContent = note.title || "Task update";
    const body = document.createElement("p");
    body.textContent = note.body;
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.id = note.id;
    button.textContent = "Mark read";
    button.addEventListener("click", () => {
      session?.live?.ack([note.id]);
    });
    card.append(title, body, button);
    ui.notifications.append(card);
  }
  renderState();
}

function renderTasks(tasks) {
  if (!Array.isArray(tasks)) return;
  taskRevision++;
  const expanded = new Set([...ui.tasks.querySelectorAll("details[open]")].map(item => item.dataset.id));
  ui.tasks.replaceChildren();
  ui["task-count"].textContent = String(tasks.length);
  if (!tasks.length) {
    const empty = document.createElement("div");
    empty.className = "tasks-empty";
    const title = document.createElement("p");
    title.textContent = "Nothing on the go. Yet.";
    const body = document.createElement("span");
    body.textContent = "Ask Talker to take something off your plate.";
    empty.append(title, body);
    ui.tasks.append(empty);
  }
  for (const task of tasks.slice(0, MAX_TASKS)) {
    if (!task || typeof task !== "object") continue;
    const card = document.createElement("details");
    card.className = "task-card";
    card.dataset.id = clip(task.id, 256);
    card.open = expanded.has(card.dataset.id);
    const summary = document.createElement("summary");
    summary.textContent = clip(task.prompt, 300) || "Background task";
    const status = document.createElement("span");
    status.className = "task-status";
    status.dataset.status = clip(task.status, 40).toLowerCase();
    status.textContent = clip(task.status, 40) || "Pending";
    summary.append(status);
    const output = document.createElement("p");
    output.className = "task-output";
    output.textContent = clip(task.error || task.output, 16000) || "Updates will appear here as the task progresses.";
    card.append(summary, output);
    ui.tasks.append(card);
  }
  if (tasks.length > MAX_TASKS) {
    const more = document.createElement("p");
    more.className = "sidebar-empty";
    more.textContent = `Showing ${MAX_TASKS} of ${tasks.length} tasks.`;
    ui.tasks.append(more);
  }
}

async function getJSON(path) {
  const response = await fetch(path, { cache: "no-store", signal: AbortSignal.timeout(5000) });
  if (!response.ok) throw new Error("Snapshot unavailable");
  return response.json();
}

async function refreshSnapshots() {
  if (session || snapshotPending || document.visibilityState !== "visible") return;
  snapshotPending = true;
  const taskAtStart = taskRevision;
  const notificationAtStart = notificationRevision;
  try {
    await Promise.allSettled([
      getJSON("/api/tasks").then(data => { if (!session && taskAtStart === taskRevision) renderTasks(data.tasks); }),
      getJSON("/api/events").then(data => {
        if (!session && notificationAtStart === notificationRevision) renderEventSnapshot(data.events);
      }),
    ]);
  } finally { snapshotPending = false; }
}

$("start-button").addEventListener("click", () => { void startSession(); });
$("stop-button").addEventListener("click", () => {
  if (session) closeSession(session, "Session ended. Your microphone is off; background tasks keep running.");
});
$("mute-button").addEventListener("click", () => {
  const s = session;
  if (s?.phase !== "connected") return;
  s.muted = !s.muted;
  setCapture(s, !s.muted);
  s.live.setMuted(s.muted);
  renderState();
});
$("text-form").addEventListener("submit", event => {
  event.preventDefault();
  const text = ui["text-input"].value.trim();
  if (!text || session?.phase !== "connected") return;
  if (session.live.sendText(text)) {
    ui["text-input"].value = "";
    session.thinking = true;
    renderState();
  }
});
$("clear-transcript").addEventListener("click", () => {
  drafts.clear();
  for (const entry of ui.transcript.querySelectorAll(".transcript-entry")) entry.remove();
  ui["transcript-empty"].hidden = false;
});
$("settings-toggle").addEventListener("click", event => {
  const panel = $("settings-panel");
  panel.hidden = !panel.hidden;
  event.currentTarget.setAttribute("aria-expanded", String(!panel.hidden));
});
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") {
    if (session?.phase === "connected") void requestWakeLock(session);
    void refreshSnapshots();
  } else if (session) {
    message("status", "Talker can listen while you work in another tab. Keep this tab open and your laptop awake; browser background or sleep policies can pause audio.");
  }
});
window.addEventListener("pagehide", () => { if (session) closeSession(session); });
window.addEventListener("offline", () => { if (session) fail(session, new Error("The network went offline. Your microphone is off. Start again after reconnecting.")); });

void getJSON("/api/config").then(data => {
  if (session) return;
  if (data.model) ui["setting-model"].textContent = clip(data.model, 200);
  if (data.voice) ui["setting-voice"].textContent = clip(data.voice, 100);
}).catch(() => {});
void refreshSnapshots();
setInterval(() => { void refreshSnapshots(); }, 10000);
