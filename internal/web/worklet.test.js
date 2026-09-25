import test from "node:test";
import assert from "node:assert/strict";
import { MIC_CREDITS, decodePCM16 } from "./assets/audio.js";

let Processor;
globalThis.sampleRate = 48000;
globalThis.AudioWorkletProcessor = class {
  constructor() {
    this.messages = [];
    this.port = { postMessage: message => this.messages.push(message) };
  }
};
globalThis.registerProcessor = (name, processor) => {
  assert.equal(name, "talker-microphone");
  Processor = processor;
};
await import("./assets/mic-worklet.js");

function configure(processor, enabled, epoch = 1) {
  processor.port.onmessage({ data: { type: "configure", enabled, epoch } });
}

function render(processor, count, level, credit = false) {
  const quantum = new Float32Array(128).fill(level);
  for (let i = 0; i < count; i++) {
    const before = processor.messages.length;
    processor.process([[quantum]]);
    if (credit) {
      for (const message of processor.messages.slice(before)) {
        if (message.type === "frame") processor.port.onmessage({ data: { type: "credit", epoch: message.epoch } });
      }
    }
  }
}

test("microphone starts disabled and emits exact 20ms PCM frames when enabled", () => {
  const processor = new Processor();
  render(processor, 100, 0.1);
  assert.equal(processor.messages.length, 0);
  configure(processor, true);
  render(processor, 40, 0.1, true);
  const frames = processor.messages.filter(message => message.type === "frame");
  assert.ok(frames.length >= 5);
  assert.ok(frames.every(frame => frame.pcm.byteLength === 640 && frame.epoch === 1));
});

test("worklet stops instead of growing an unbounded message backlog", () => {
  const processor = new Processor();
  configure(processor, true);
  render(processor, 300, 0.1);
  assert.equal(processor.messages.filter(message => message.type === "frame").length, MIC_CREDITS);
  assert.equal(processor.messages.filter(message => message.type === "overflow").length, 1);
  assert.equal(processor.enabled, false);
  render(processor, 100, 0.1);
  assert.equal(processor.messages.length, MIC_CREDITS + 1);
});

test("mute and restart discard partial frames, FIR history, and obsolete credits", () => {
  const processor = new Processor();
  configure(processor, true);
  render(processor, 20, 0.8, true);
  configure(processor, false, 2);
  processor.messages.length = 0;
  render(processor, 100, 0.8);
  assert.equal(processor.messages.length, 0);
  configure(processor, true, 3);
  processor.credits = 2;
  processor.port.onmessage({ data: { type: "credit", epoch: 1 } });
  assert.equal(processor.credits, 2);
  render(processor, 10, 0, true);
  assert.equal(processor.messages[0].epoch, 3);
  assert.ok(decodePCM16(processor.messages[0].pcm).every(sample => sample === 0));
  assert.equal(processor.messages[0].speaking, false);
});

test("local activity has a quiet hold but never emits an interruption command", () => {
  const processor = new Processor();
  configure(processor, true);
  render(processor, 20, 0.1, true);
  assert.equal(processor.messages.at(-1).speaking, true);
  render(processor, 20, 0, true);
  assert.equal(processor.messages.at(-1).speaking, true);
  render(processor, 110, 0, true);
  assert.equal(processor.messages.at(-1).speaking, false);
  assert.ok(processor.messages.every(message => message.type === "frame"));
});

test("multiple input channels are downmixed before encoding", () => {
  const processor = new Processor();
  configure(processor, true);
  for (let i = 0; i < 10; i++) processor.process([[new Float32Array(128).fill(0.5), new Float32Array(128).fill(-0.5)]]);
  assert.equal(processor.messages.length, 1);
  assert.ok(decodePCM16(processor.messages[0].pcm).every(sample => sample === 0));
});
