import test from "node:test";
import assert from "node:assert/strict";
import { OUTPUT_RATE } from "./assets/audio.js";

// Provide the AudioWorkletGlobalScope surface the processor expects.
const registered = new Map();
class FakePort {
  constructor() { this.sent = []; this.onmessage = null; }
  postMessage(data) { this.sent.push(data); }
  receive(data) { this.onmessage?.({ data }); }
}
globalThis.AudioWorkletProcessor = class { constructor() { this.port = new FakePort(); } };
globalThis.registerProcessor = (name, cls) => registered.set(name, cls);
globalThis.sampleRate = 48000;
await import("./assets/playback-worklet.js");
const Playback = registered.get("talker-playback");

function processor() { return new Playback(); }
function quantum(p, frames = 128) {
  const out = new Float32Array(frames);
  p.process([], [[out]]);
  return out;
}
function tone(seconds, hz = 440) {
  const n = Math.round(seconds * OUTPUT_RATE);
  const pcm = new Int16Array(n);
  for (let i = 0; i < n; i++) pcm[i] = Math.round(Math.sin((2 * Math.PI * hz * i) / OUTPUT_RATE) * 16000);
  return pcm.buffer;
}

test("worklet drains 24kHz speech at real time on a 48kHz device and reports activity edges", () => {
  const p = processor();
  assert.deepEqual(quantum(p), new Float32Array(128), "silence before any audio");
  p.port.receive({ type: "push", pcm: tone(0.5), epoch: 0 });
  let quanta = 0;
  let energy = 0;
  while (p.queue.queuedSamples > 0 || p.pending.length || p.draining > 0) {
    const out = quantum(p);
    for (const v of out) energy += v * v;
    quanta++;
    assert.ok(quanta < 400, "must drain in ~real time, not stall");
  }
  // 0.5 s at 48 kHz is 24000 frames = 187.5 quanta of 128.
  assert.ok(quanta >= 187 && quanta <= 190, `drained in ${quanta} quanta`);
  assert.ok(energy > 100, "tone must actually reach the output");
  const edges = p.port.sent.filter(m => m.type === "active").map(m => m.active);
  assert.deepEqual(edges, [true, false]);
});

test("worklet output is continuous across chunk boundaries and quantum boundaries", () => {
  const p = processor();
  // Two adjacent chunks of one continuous low-frequency ramp.
  const total = Math.round(OUTPUT_RATE * 0.1);
  const pcm = new Int16Array(total);
  for (let i = 0; i < total; i++) pcm[i] = Math.round(((i / total) * 2 - 1) * 20000);
  p.port.receive({ type: "push", pcm: pcm.buffer.slice(0, 1000), epoch: 0 });
  p.port.receive({ type: "push", pcm: pcm.buffer.slice(1000), epoch: 0 });
  const samples = [];
  while (p.queue.queuedSamples > 0 || p.pending.length || p.draining > 0) samples.push(...quantum(p));
  // Skip the filter's response to the ramp's own hard onset and terminal edge; every
  // step of the smooth interior must be tiny, including across the chunk seam. The
  // sinc spans `taps` input samples, i.e. taps × (48k/24k) output samples.
  const contentEnd = total * (sampleRate / OUTPUT_RATE);
  const edge = p.resampler.taps * (sampleRate / OUTPUT_RATE) + 8;
  let maxStep = 0;
  for (let i = edge; i < contentEnd - edge; i++) maxStep = Math.max(maxStep, Math.abs(samples[i] - samples[i - 1]));
  assert.ok(maxStep < 0.002, `discontinuity ${maxStep} detected at a seam`);
  // The chunk seam (source sample 500 → output ≈ 1000 at 2× rate) must be inside
  // the checked interior, so the assertion above genuinely covers it.
  assert.ok(edge < 900 && contentEnd - edge > 1100, "seam must lie within the checked region");
  // The tail must not be clipped by the resampler's group delay: the ramp's final
  // value (+20000/32768 ≈ 0.61) has to actually reach the output.
  const peak = Math.max(...samples.slice(-200));
  assert.ok(peak > 0.6, `utterance tail was clipped; peak near end ${peak}`);
  assert.equal(samples.at(-1), 0, "output settles to silence after the tail");
});

test("flush discards queued audio immediately and ignores stale-epoch pushes", () => {
  const p = processor();
  p.port.receive({ type: "push", pcm: tone(30), epoch: 0 });
  quantum(p);
  assert.ok(p.queue.queuedSeconds > 29);
  p.port.receive({ type: "flush", epoch: 1 });
  assert.equal(p.queue.queuedSamples, 0);
  assert.deepEqual(quantum(p), new Float32Array(128), "silence right after interruption");
  p.port.receive({ type: "push", pcm: tone(1), epoch: 0 });
  assert.equal(p.queue.queuedSamples, 0, "audio from before the interruption must not play");
  p.port.receive({ type: "push", pcm: tone(1), epoch: 1 });
  assert.ok(p.queue.queuedSamples > 0);
});

test("a full queue is reported as an error rather than silently dropped", () => {
  const p = processor();
  p.queue.maxSamples = 10;
  p.port.receive({ type: "push", pcm: tone(1), epoch: 0 });
  const err = p.port.sent.find(m => m.type === "error");
  assert.match(err.message, /queue is full/);
});
