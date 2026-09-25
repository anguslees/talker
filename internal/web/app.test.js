import test from "node:test";
import assert from "node:assert/strict";

class Element {
  constructor(tag = "div") {
    this.tag = tag;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.listeners = {};
    this.className = "";
    this.classList = { toggle() {} };
    this.value = "";
    this.scrollHeight = this.scrollTop = this.clientHeight = 0;
    this._text = "";
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map(child => child.textContent).join(""); }
  set innerHTML(_) { throw new Error("Untrusted HTML must never be inserted"); }
  append(...children) { for (const child of children) { child.parent = this; this.children.push(child); } }
  replaceChildren(...children) { this.children = []; this.append(...children); }
  remove() { this.parent.children = this.parent.children.filter(child => child !== this); }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener(type, listener) { this.listeners[type] = listener; }
  emit(type, event = {}) { this.listeners[type]?.({ currentTarget: this, preventDefault() {}, ...event }); }
  querySelectorAll(selector) {
    const matches = child => selector.startsWith(".") ? child.className.split(" ").includes(selector.slice(1)) : selector === "details[open]" ? child.tag === "details" && child.open : child.tag === selector;
    return this.children.flatMap(child => [...(matches(child) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

const settle = async () => { for (let i = 0; i < 40; i++) await Promise.resolve(); };
const audioMessage = () => ({ serverContent: { modelTurn: { parts: [{ inlineData: { mimeType: "audio/pcm;rate=24000", data: btoa("\0".repeat(4800)) } }] } } });
let caseID = 0;

async function browser(t, options = {}) {
  const elements = new Map();
  const get = id => {
    if (!elements.has(id)) elements.set(id, new Element());
    return elements.get(id);
  };
  get("mute-button").append(new Element("span"));
  get("transcript").append(get("transcript-empty"));
  const document = { getElementById: get, createElement: tag => new Element(tag), visibilityState: "visible", listeners: {}, addEventListener(type, listener) { this.listeners[type] = listener; } };
  const env = { get, contexts: [], sockets: [], microphones: [], players: [], captureRequests: [], operations: [], timers: new Map() };
  env.flushes = () => env.players[0]?.controls.filter(d => d.type === "flush").length ?? 0;
  env.pushes = () => env.players[0]?.controls.filter(d => d.type === "push").length ?? 0;
  env.document = document;
  const track = { enabled: true, stops: 0, stop() { this.stops++; } };
  env.track = track;
  env.stream = { getTracks: () => [track], getAudioTracks: () => [track] };
  const node = () => ({ connect() {}, disconnect() { this.disconnected = true; } });
  class Context {
    constructor() { this.sampleRate = 44100; this.state = "suspended"; this.currentTime = 0; this.sources = []; this.destination = {}; this.audioWorklet = { addModule: async () => {} }; env.contexts.push(this); }
    resume() { env.operations.push("resume"); this.state = "running"; return Promise.resolve(); }
    close() { this.state = "closed"; return Promise.resolve(); }
    createGain() { return { ...node(), gain: { value: 1, cancelScheduledValues() {}, setValueAtTime(v) { this.value = v; }, linearRampToValueAtTime(v) { this.value = v; } } }; }
    createMediaStreamSource() { env.operations.push("capture-node"); return node(); }
    createBuffer(_, length, sampleRate) { return { length, sampleRate, copyToChannel() {} }; }
    createBufferSource() {
      const source = { ...node(), start() {}, stop() { this.stopped = true; } };
      this.sources.push(source);
      return source;
    }
  }
  // One fake AudioWorkletNode class serves both processors; the name selects behaviour.
  class Microphone {
    constructor(_context, name) {
      this.name = name;
      this.controls = [];
      this.port = { postMessage: data => this.controls.push(data), close() { this.closed = true; } };
      if (name === "talker-playback") env.players.push(this); else env.microphones.push(this);
    }
    // Playback fakes: the worklet reports drain edges back to the page.
    drained() { this.port.onmessage?.({ data: { type: "active", active: false, epoch: this.controls.filter(d => d.type === "flush").length } }); }
    connect() {}
    disconnect() {}
    frame(overrides = {}) {
      this.port.onmessage?.({ data: { type: "frame", epoch: this.controls.filter(data => data.type === "configure").at(-1).epoch, speaking: false, pcm: new ArrayBuffer(640), ...overrides } });
    }
  }
  class Socket {
    static OPEN = 1;
    constructor(url) { this.url = new URL(url); this.readyState = 0; this.sent = []; this.bufferedAmount = 0; env.operations.push("socket"); env.sockets.push(this); }
    send(data) { this.sent.push(typeof data === "string" ? JSON.parse(data) : data); }
    close() { this.readyState = 3; this.closed = true; this.onclose?.(); }
    receive(data) { this.onmessage?.({ data: data instanceof ArrayBuffer ? data : JSON.stringify(data) }); }
    open() { this.readyState = 1; this.onopen?.(); }
    ready() { this.open(); this.receive(this.url.pathname === "/api/control" ? { type: "control_ready" } : { setupComplete: {} }); }
  }
  const globals = {
    document,
    window: { isSecureContext: true, AudioContext: Context, AudioWorkletNode: Microphone, WebSocket: Socket, location: { href: "http://localhost:8080/", protocol: "http:" }, addEventListener() {} },
    navigator: { mediaDevices: { getUserMedia: constraints => { env.captureRequests.push(constraints); return options.capture ? options.capture(env) : Promise.resolve(env.stream); } } },
    AudioWorkletNode: Microphone,
    WebSocket: Socket,
    fetch: async (path, request) => ({ ok: true, json: async () => {
      if (path === "/api/live/token") return { url: "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained?access_token=test-token", setup: { model: "models/test-model", sessionResumption: JSON.parse(request.body).handle ? { handle: JSON.parse(request.body).handle } : {} }, model: "test-model", voice: "test-voice" };
      return path === "/api/tasks" ? { tasks: [] } : path === "/api/events" ? { events: [] } : { model: "test-model", voice: "test-voice", transport: "browser-direct" };
    } }),
    setTimeout: fn => { const id = Symbol(); env.timers.set(id, fn); return id; },
    setInterval: fn => { const id = Symbol(); env.timers.set(id, fn); return id; },
    clearTimeout: id => env.timers.delete(id),
    clearInterval: id => env.timers.delete(id),
  };
  const originals = new Map();
  for (const [name, value] of Object.entries(globals)) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
  }
  t.after(async () => {
    get("stop-button").emit("click");
    await settle();
    for (const [name, descriptor] of originals) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  });
  await import(`./assets/app.js?test=${++caseID}`);
  await settle();
  env.start = async () => {
    get("start-button").emit("click");
    await settle();
    env.sockets.at(-1).ready();
    await settle();
    env.sockets.at(-1).ready();
    await settle();
  };
  return env;
}

test("browser resumes audio in the click gesture and waits for control and Google setup before capture", async t => {
  const env = await browser(t);
  env.get("start-button").emit("click");
  assert.deepEqual(env.operations, ["resume"]);
  await settle();
  assert.equal(env.captureRequests.length, 0);
  assert.equal(env.sockets[0].url.href, "ws://localhost:8080/api/control");
  env.sockets[0].ready();
  await settle();
  assert.equal(env.captureRequests.length, 0);
  assert.equal(env.sockets[1].url.origin, "wss://generativelanguage.googleapis.com");
  env.sockets[1].ready();
  await settle();
  assert.equal(env.captureRequests.length, 1);
  assert.deepEqual(env.captureRequests[0].audio, { channelCount: { ideal: 1 }, echoCancellation: true, noiseSuppression: true, autoGainControl: true });
  assert.equal(env.get("connection-label").textContent, "Live / direct");
  assert.match(env.get("setting-input").textContent, /44,100/);
  env.get("stop-button").emit("click");
  assert.equal(env.track.stops, 1);
  assert.equal(env.contexts[0].state, "closed");
  assert.equal(env.microphones[0].port.closed, true);
  assert.equal(env.sockets[0].closed, true);
});

test("stopping during microphone permission closes late-arriving tracks without attaching capture", async t => {
  let allow;
  const env = await browser(t, { capture: () => new Promise(resolve => { allow = resolve; }) });
  await env.start();
  env.get("stop-button").emit("click");
  allow(env.stream);
  await settle();
  assert.equal(env.track.stops, 1);
  assert.equal(env.microphones.length, 0);
  assert.equal(env.get("connection-label").textContent, "Offline");
});

test("microphone permission failure releases the connection and requires an explicit restart", async t => {
  const env = await browser(t, { capture: () => Promise.reject(new DOMException("Denied", "NotAllowedError")) });
  await env.start();
  assert.match(env.get("error-message").textContent, /access was not granted/);
  assert.equal(env.sockets[0].closed, true);
  assert.equal(env.contexts[0].state, "closed");
  assert.equal(env.sockets.length, 2);
  assert.equal(env.get("start-button").disabled, false);
});

test("microphone network backpressure fails closed instead of buffering audio", async t => {
  const env = await browser(t);
  await env.start();
  env.sockets[1].bufferedAmount = 8000;
  env.microphones[0].frame();
  assert.match(env.get("error-message").textContent, /cannot keep up/);
  assert.equal(env.track.stops, 1);
  assert.equal(env.sockets[1].sent.filter(message => message.realtimeInput?.audio).length, 0);
  assert.equal(env.sockets[0].sent.length, 0);
});

test("mute resets input epochs and signals stream end without stopping model playback", async t => {
  const env = await browser(t);
  await env.start();
  const socket = env.sockets[1];
  const mic = env.microphones[0];
  socket.receive(audioMessage());
  await settle();
  const oldEpoch = mic.controls.at(-1).epoch;
  env.get("mute-button").emit("click");
  assert.equal(env.track.enabled, false);
  assert.equal(mic.controls.at(-1).enabled, false);
  assert.deepEqual(socket.sent.at(-1), { realtimeInput: { audioStreamEnd: true } });
  assert.equal(env.flushes(), 0, "muting the mic must not flush Talker's speech");
  env.get("mute-button").emit("click");
  const sent = socket.sent.length;
  mic.frame({ epoch: oldEpoch });
  assert.equal(socket.sent.length, sent);
  mic.frame();
  assert.equal(socket.sent.at(-1).realtimeInput.audio.mimeType, "audio/pcm;rate=16000");
  assert.equal(atob(socket.sent.at(-1).realtimeInput.audio.data).length, 640);
});

test("local activity and turn completion do not interrupt actual queued playback", async t => {
  const env = await browser(t);
  await env.start();
  const socket = env.sockets[1];
  socket.receive(audioMessage());
  await settle();
  env.microphones[0].frame({ speaking: true });
  socket.receive({ serverContent: { turnComplete: true } });
  await settle();
  assert.equal(env.pushes(), 1, "audio is handed to the playback worklet");
  assert.equal(env.flushes(), 0);
  assert.equal(env.get("voice-console").dataset.state, "speaking");
  socket.receive({ serverContent: { interrupted: true } });
  await settle();
  assert.equal(env.flushes(), 1, "barge-in flushes queued speech immediately");
  assert.equal(env.sockets[0].sent.length, 0);
});

test("GoAway pauses capture visibly, resets playback, and resumes only on setupComplete", async t => {
  const env = await browser(t);
  await env.start();
  const socket = env.sockets[1];
  socket.receive(audioMessage());
  socket.receive({ sessionResumptionUpdate: { resumable: true, newHandle: "resume-test" }, goAway: { timeLeft: "0s" } });
  await settle();
  for (const timer of [...env.timers.values()]) timer();
  await settle();
  assert.equal(env.track.enabled, false);
  assert.equal(env.get("connection-label").textContent, "Reconnecting");
  assert.ok(env.flushes() >= 1, "reconnect discards queued speech");
  const count = socket.sent.length;
  env.microphones[0].frame();
  assert.equal(socket.sent.length, count);
  env.sockets.at(-1).ready();
  await settle();
  assert.equal(env.track.enabled, true);
  assert.equal(env.get("connection-label").textContent, "Live / direct");
});

test("transcript uses text, bounds DOM growth, and acknowledgements require a click", async t => {
  const env = await browser(t);
  await env.start();
  const socket = env.sockets[1];
  for (let i = 0; i < 100; i++) {
    socket.receive({ serverContent: { inputTranscription: { text: `<script>${i}</script>`, finished: true } } });
    await settle();
  }
  assert.equal(env.get("transcript").querySelectorAll(".transcript-entry").length, 80);
  assert.match(env.get("transcript").textContent, /<script>99<\/script>/);
  const control = env.sockets[0];
  control.receive({ type: "events", events: [{ id: "event-1", title: "Finished", body: "<img onerror=alert(1)>" }] });
  assert.equal(control.sent.filter(message => message.type === "ack").length, 0);
  assert.match(env.get("notifications").textContent, /<img onerror=alert\(1\)>/);
  env.get("notifications").querySelector("button").emit("click");
  assert.deepEqual(control.sent.at(-1), { type: "ack", ids: ["event-1"] });
  assert.equal(env.get("notification-count").textContent, "1");
  control.receive({ type: "acknowledged", ids: ["event-1"] });
  assert.equal(env.get("notification-count").textContent, "0");
});

test("losing the control socket closes direct audio and stops microphone tracks", async t => {
  const env = await browser(t);
  await env.start();
  env.sockets[0].close();
  await settle();
  assert.equal(env.track.stops, 1);
  assert.equal(env.sockets[1].closed, true);
  assert.match(env.get("error-message").textContent, /task control connection closed/);
});

test("switching tabs does not artificially pause microphone capture", async t => {
  const env = await browser(t);
  await env.start();
  env.document.visibilityState = "hidden";
  env.document.listeners.visibilitychange();
  assert.equal(env.track.enabled, true);
  assert.equal(env.track.stops, 0);
  assert.equal(env.get("connection-label").textContent, "Live / direct");
  env.microphones[0].frame();
  assert.ok(env.sockets[1].sent.at(-1).realtimeInput.audio);
});
