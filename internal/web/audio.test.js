import test from "node:test";
import assert from "node:assert/strict";
import { decodePCM16, encodePCM16, PCM16Framer, StreamingResampler, PlaybackQueue, OUTPUT_RATE, MAX_QUEUED_PLAYBACK_SECONDS } from "./assets/audio.js";

function tone(rate, frequency, duration = 0.2) {
  return Float32Array.from({ length: Math.round(rate * duration) }, (_, i) => Math.sin(2 * Math.PI * frequency * i / rate));
}

function rms(samples, offset = 100) {
  let sum = 0;
  for (let i = offset; i < samples.length; i++) sum += samples[i] * samples[i];
  return Math.sqrt(sum / (samples.length - offset));
}

test("PCM16 encoding clamps samples and uses little-endian signed bytes", () => {
  const encoded = encodePCM16(new Float32Array([-2, -1, -0.5, 0, 0.5, 1, 2, NaN]));
  assert.deepEqual([...new Uint8Array(encoded)], [0, 128, 0, 128, 0, 192, 0, 0, 0, 64, 255, 127, 255, 127, 0, 0]);
  const decoded = decodePCM16(encoded);
  assert.equal(decoded[0], -1);
  assert.equal(decoded[2], -0.5);
  assert.equal(decoded[4], 0.5);
  assert.ok(Math.abs(decoded[5] - 1) < 1 / 32767);
  assert.equal(decoded[7], 0);
});

test("PCM16 decoding respects view offsets and rejects partial samples", () => {
  const bytes = new Uint8Array([1, 2, 0, 128, 255, 127, 3, 4]);
  assert.deepEqual([...decodePCM16(bytes.subarray(2, 6))], [-1, 32767 / 32768]);
  assert.throws(() => decodePCM16(new ArrayBuffer(3)), /complete samples/);
});

test("framing emits exact 20ms packets and discards partial input on reset", () => {
  const framer = new PCM16Framer();
  assert.equal(framer.push(new Float32Array(200).fill(0.5)).length, 0);
  const frames = framer.push(new Float32Array(760).fill(0.5));
  assert.equal(frames.length, 3);
  for (const frame of frames) {
    assert.equal(frame.pcm.byteLength, 640);
    assert.equal(frame.rms, 0.5);
    assert.equal(decodePCM16(frame.pcm)[0], 0.5);
  }
  framer.push(new Float32Array(319).fill(1));
  framer.reset();
  assert.equal(framer.push(new Float32Array(1)).length, 0);
  const after = framer.push(new Float32Array(319));
  assert.equal(after[0].rms, 0);
  assert.equal(rms(decodePCM16(after[0].pcm)), 0);
});

for (const rate of [8000, 16000, 32000, 44100, 48000, 96000, 192000]) {
  test(`resampler preserves state across arbitrary ${rate}Hz input chunks`, () => {
    const input = tone(rate, 1000, 0.1);
    const contiguous = new StreamingResampler(rate).push(input);
    const chunked = new StreamingResampler(rate);
    const result = [];
    let offset = 0;
    for (let chunk = 0; offset < input.length; chunk++) {
      const size = [128, 33, 256, 1, 127][chunk % 5];
      result.push(...chunked.push(input.subarray(offset, offset + size)));
      offset += size;
    }
    assert.deepEqual(result, [...contiguous]);
    const expected = (input.length - chunked.half) / chunked.step;
    assert.ok(Math.abs(result.length - expected) <= 1, `${result.length} samples; expected about ${expected}`);
    assert.ok(rms(contiguous) > 0.69);
    assert.ok(rms(contiguous) < 0.72);
    assert.ok(chunked.history.length < 500);
  });
}

for (const rate of [44100, 48000, 96000]) {
  test(`resampler suppresses aliasing before downsampling ${rate}Hz`, () => {
    const passband = new StreamingResampler(rate).push(tone(rate, 1000));
    const stopband = new StreamingResampler(rate).push(tone(rate, 11000));
    assert.ok(rms(passband) > 0.69);
    assert.ok(rms(stopband) < 0.002, `Stopband RMS ${rms(stopband)}`);
  });
}

test("resampler reset clears history, phase, and startup delay", () => {
  const converter = new StreamingResampler(44100);
  converter.push(tone(44100, 700));
  converter.reset();
  const after = converter.push(new Float32Array(4410));
  const fresh = new StreamingResampler(44100).push(new Float32Array(4410));
  assert.deepEqual(after, fresh);
  assert.equal(rms(after), 0);
});

test("resampler rejects invalid sample rates and preserves DC gain", () => {
  for (const rate of [NaN, Infinity, 0, -1]) assert.throws(() => new StreamingResampler(rate), /sample rate/);
  const output = new StreamingResampler(48000).push(new Float32Array(4800).fill(1));
  for (const value of output.subarray(100)) assert.ok(Math.abs(value - 1) < 1e-5);
});

// A 24 kHz ramp lets tests verify sample order and continuity across chunk seams.
function ramp(samples, start = 0) {
  const pcm = new Int16Array(samples);
  for (let i = 0; i < samples; i++) pcm[i] = ((start + i) % 2000) - 1000;
  return pcm.buffer;
}

test("playback queue drains pushed audio in order and in exact amounts", () => {
  const queue = new PlaybackQueue();
  queue.push(ramp(100));
  queue.push(ramp(50, 100));
  assert.equal(queue.queuedSamples, 150);
  const out = new Float32Array(128);
  assert.equal(queue.pull(out), 128);
  assert.equal(out[0], -1000 / 32768);
  assert.equal(out[127], (127 - 1000) / 32768, "chunk seam must be seamless");
  assert.equal(queue.queuedSamples, 22);
  assert.equal(queue.pull(out), 22);
  assert.equal(out[21], (149 - 1000) / 32768);
  assert.equal(out[22], 0, "underrun is zero-filled silence, not stale audio");
  assert.equal(queue.queuedSamples, 0);
});

// The Live API has no backpressure; Gemini sends a reply several times faster
// than real time. A long reading must simply queue and drain, never fault.
test("a long fast-arriving reply queues without error and drains at real time", () => {
  const queue = new PlaybackQueue();
  const chunk = ramp(Math.round(OUTPUT_RATE * 0.04)); // 40 ms Live chunk
  const chunks = Math.round(180 / 0.04); // three minutes of reading text aloud
  for (let i = 0; i < chunks; i++) queue.push(chunk.slice(0));
  assert.ok(Math.abs(queue.queuedSeconds - 180) < 0.05);
  const quantum = new Float32Array(128);
  let quanta = 0;
  while (queue.queuedSamples > 0) { queue.pull(quantum); quanta++; }
  assert.equal(quanta, Math.ceil((180 * OUTPUT_RATE) / 128));
});

test("playback queue bounds only memory, and interruption flush discards everything", () => {
  const queue = new PlaybackQueue(1);
  queue.push(ramp(OUTPUT_RATE));
  assert.throws(() => queue.push(ramp(1)), /queue is full/);
  queue.flush();
  assert.equal(queue.queuedSamples, 0);
  queue.push(ramp(10));
  const out = new Float32Array(4);
  assert.equal(queue.pull(out), 4);
  assert.equal(new PlaybackQueue().maxSamples, MAX_QUEUED_PLAYBACK_SECONDS * OUTPUT_RATE);
  assert.throws(() => queue.push(new ArrayBuffer(3)), /complete samples/);
  assert.throws(() => new PlaybackQueue(0), /Invalid/);
});

test("playback queue copies input so transferred buffers can be released", () => {
  const queue = new PlaybackQueue();
  const source = ramp(8);
  queue.push(source);
  new Int16Array(source).fill(0);
  const out = new Float32Array(8);
  queue.pull(out);
  assert.equal(out[0], -1000 / 32768);
});
