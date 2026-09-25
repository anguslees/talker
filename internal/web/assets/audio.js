export const INPUT_RATE = 16000;
export const OUTPUT_RATE = 24000;
export const FRAME_SAMPLES = 320;
export const MIC_CREDITS = 8;
export const MAX_MIC_BUFFERED_BYTES = 8000;
// Gemini has no playback backpressure: a reply's audio arrives several times
// faster than real time and the client drains it at real time. Depth is therefore
// normal; this ceiling only protects memory against a runaway stream.
export const MAX_QUEUED_PLAYBACK_SECONDS = 600;

export function encodePCM16(samples) {
  const buffer = new ArrayBuffer(samples.length * 2);
  const view = new DataView(buffer);
  for (let i = 0; i < samples.length; i++) {
    const value = Math.max(-1, Math.min(1, Number.isFinite(samples[i]) ? samples[i] : 0));
    view.setInt16(i * 2, Math.round(value * (value < 0 ? 32768 : 32767)), true);
  }
  return buffer;
}

export function decodePCM16(buffer) {
  const view = ArrayBuffer.isView(buffer)
    ? new DataView(buffer.buffer, buffer.byteOffset, buffer.byteLength)
    : new DataView(buffer);
  if (view.byteLength % 2 !== 0) throw new Error("PCM16 audio must contain complete samples.");
  const samples = new Float32Array(view.byteLength / 2);
  for (let i = 0; i < samples.length; i++) samples[i] = view.getInt16(i * 2, true) / 32768;
  return samples;
}

export class PCM16Framer {
  constructor(size = FRAME_SAMPLES) {
    if (!Number.isInteger(size) || size < 1) throw new Error("Invalid audio frame size.");
    this.size = size;
    this.reset();
  }

  reset() {
    this.buffer = new ArrayBuffer(this.size * 2);
    this.view = new DataView(this.buffer);
    this.length = 0;
    this.energy = 0;
  }

  push(samples) {
    const frames = [];
    for (let i = 0; i < samples.length; i++) {
      const value = Math.max(-1, Math.min(1, Number.isFinite(samples[i]) ? samples[i] : 0));
      this.view.setInt16(this.length * 2, Math.round(value * (value < 0 ? 32768 : 32767)), true);
      this.energy += value * value;
      if (++this.length === this.size) {
        frames.push({ pcm: this.buffer, rms: Math.sqrt(this.energy / this.size) });
        this.reset();
      }
    }
    return frames;
  }
}

// A windowed-sinc fractional-delay filter retains history across render quanta.
export class StreamingResampler {
  constructor(inputRate, outputRate = INPUT_RATE) {
    if (![inputRate, outputRate].every(rate => Number.isFinite(rate) && rate >= 1000 && rate <= 384000)) {
      throw new Error("Unsupported audio sample rate.");
    }
    this.step = inputRate / outputRate;
    this.half = Math.ceil(16 * Math.max(1, this.step));
    this.phases = 256;
    this.taps = this.half * 2;
    this.history = new Float32Array(this.taps + Math.ceil(this.step) + 2);
    this.kernels = new Float32Array(this.phases * this.taps);
    const cutoff = 0.45 * Math.min(1, outputRate / inputRate);
    for (let phase = 0; phase < this.phases; phase++) {
      let sum = 0;
      for (let tap = 0; tap < this.taps; tap++) {
        const distance = tap - this.half + 1 - phase / this.phases;
        const x = 2 * cutoff * distance;
        const sinc = Math.abs(x) < 1e-12 ? 1 : Math.sin(Math.PI * x) / (Math.PI * x);
        const window = 0.42 + 0.5 * Math.cos(Math.PI * distance / this.half) + 0.08 * Math.cos(2 * Math.PI * distance / this.half);
        const value = 2 * cutoff * sinc * window;
        this.kernels[phase * this.taps + tap] = value;
        sum += value;
      }
      for (let tap = 0; tap < this.taps; tap++) this.kernels[phase * this.taps + tap] /= sum;
    }
    this.reset();
  }

  reset() {
    this.history.fill(0);
    this.written = 0;
    this.next = 0;
  }

  push(input) {
    const output = new Float32Array(Math.ceil(input.length / this.step) + 1);
    let length = 0;
    for (let i = 0; i < input.length; i++) {
      this.history[this.written % this.history.length] = Number.isFinite(input[i]) ? input[i] : 0;
      this.written++;
      while (Math.floor(this.next) + this.half < this.written) {
        const center = Math.floor(this.next);
        const phase = Math.min(this.phases - 1, Math.round((this.next - center) * this.phases));
        const first = center - this.half + 1;
        let value = 0;
        for (let tap = 0; tap < this.taps; tap++) {
          const position = first + tap;
          if (position >= 0) value += this.history[position % this.history.length] * this.kernels[phase * this.taps + tap];
        }
        output[length++] = value;
        this.next += this.step;
      }
    }
    return output.subarray(0, length);
  }
}

// PlaybackQueue holds 24 kHz PCM16 awaiting real-time drain. push and pull may
// run on different threads' copies; the class itself is single-threaded and pure.
export class PlaybackQueue {
  constructor(maxSeconds = MAX_QUEUED_PLAYBACK_SECONDS) {
    if (!Number.isFinite(maxSeconds) || maxSeconds <= 0) throw new Error("Invalid playback queue bound.");
    this.maxSamples = Math.floor(maxSeconds * OUTPUT_RATE);
    this.chunks = [];
    this.offset = 0;
    this.queued = 0;
  }

  get queuedSamples() { return this.queued; }
  get queuedSeconds() { return this.queued / OUTPUT_RATE; }

  push(pcm) {
    const view = ArrayBuffer.isView(pcm) ? pcm : new Uint8Array(pcm);
    if (view.byteLength % 2 !== 0) throw new Error("PCM16 audio must contain complete samples.");
    if (view.byteLength === 0) return;
    const samples = view.byteLength / 2;
    if (this.queued + samples > this.maxSamples) throw new Error("Audio playback queue is full. Start a new session.");
    // Copy so the caller's transferable buffer can be released.
    this.chunks.push(new Int16Array(view.buffer.slice(view.byteOffset, view.byteOffset + view.byteLength)));
    this.queued += samples;
  }

  // Fills out with up to out.length samples as floats, zero-filling on underrun.
  // Returns the number of real samples written.
  pull(out) {
    let written = 0;
    while (written < out.length && this.chunks.length) {
      const chunk = this.chunks[0];
      const take = Math.min(out.length - written, chunk.length - this.offset);
      for (let i = 0; i < take; i++) out[written + i] = chunk[this.offset + i] / 32768;
      written += take;
      this.offset += take;
      if (this.offset === chunk.length) { this.chunks.shift(); this.offset = 0; }
    }
    this.queued -= written;
    if (written < out.length) out.fill(0, written);
    return written;
  }

  flush() {
    this.chunks = [];
    this.offset = 0;
    this.queued = 0;
  }
}

// PCMPlayer feeds a PlaybackQueue running inside the playback worklet so draining
// continues at real time even when the page's timers are throttled.
export class PCMPlayer {
  constructor(context, onActivity = () => {}) {
    this.context = context;
    this.onActivity = onActivity;
    this.active = false;
    this.epoch = 0;
    this.node = new AudioWorkletNode(context, "talker-playback", { numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [1] });
    this.gain = context.createGain();
    this.node.connect(this.gain);
    this.gain.connect(context.destination);
    this.node.port.onmessage = ({ data }) => {
      if (data.epoch !== this.epoch) return;
      if (data.type === "active") this.setActive(data.active);
      else if (data.type === "error") this.onError?.(new Error(data.message));
    };
  }

  push(pcm) {
    if (this.context.state !== "running") throw new Error("Audio playback is paused. Start a new session.");
    if (pcm.byteLength === 0) return;
    if (pcm.byteLength % 2 !== 0) throw new Error("PCM16 audio must contain complete samples.");
    // Restore gain in case a flush faded it; ramps are cancelled by the fresh value.
    this.gain.gain.cancelScheduledValues(this.context.currentTime);
    this.gain.gain.setValueAtTime(1, this.context.currentTime);
    this.node.port.postMessage({ type: "push", pcm, epoch: this.epoch }, [pcm]);
    this.setActive(true);
  }

  setActive(active) {
    if (this.active === active) return;
    this.active = active;
    this.onActivity(active);
  }

  // Interruption: fade over 20 ms to avoid a click, then discard everything queued.
  flush() {
    const now = this.context.currentTime;
    this.gain.gain.cancelScheduledValues(now);
    this.gain.gain.setValueAtTime(this.gain.gain.value, now);
    this.gain.gain.linearRampToValueAtTime(0, now + 0.02);
    this.epoch++;
    this.node.port.postMessage({ type: "flush", epoch: this.epoch });
    this.setActive(false);
  }

  close() {
    this.flush();
    this.node.port.onmessage = null;
    this.node.port.close();
    this.node.disconnect();
    this.gain.disconnect();
  }
}
