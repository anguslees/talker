import { PlaybackQueue, StreamingResampler, OUTPUT_RATE } from "./audio.js";

// Drains queued 24 kHz speech at real time on the audio render thread, so playback
// is unaffected by main-thread timer throttling in background tabs.
class TalkerPlayback extends AudioWorkletProcessor {
  constructor() {
    super();
    this.queue = new PlaybackQueue();
    this.resampler = new StreamingResampler(OUTPUT_RATE, sampleRate);
    // Pull exactly enough 24 kHz samples to produce one render quantum at device rate.
    this.ratio = OUTPUT_RATE / sampleRate;
    this.carry = 0;
    this.draining = 0;
    this.pending = new Float32Array(0);
    this.epoch = 0;
    this.active = false;
    this.scratch = new Float32Array(256);
    this.port.onmessage = ({ data }) => {
      if (data.type === "flush") {
        this.queue.flush();
        this.resampler.reset();
        this.pending = new Float32Array(0);
        this.draining = 0;
        this.epoch = data.epoch;
        this.setActive(false);
      } else if (data.type === "push" && data.epoch === this.epoch) {
        try {
          this.queue.push(data.pcm);
        } catch (error) {
          this.port.postMessage({ type: "error", message: error.message, epoch: this.epoch });
        }
      }
    };
  }

  setActive(active) {
    if (this.active === active) return;
    this.active = active;
    this.port.postMessage({ type: "active", active, epoch: this.epoch });
  }

  process(_inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;
    let written = 0;
    // Serve any resampled samples left over from the previous quantum first.
    if (this.pending.length) {
      const take = Math.min(out.length, this.pending.length);
      out.set(this.pending.subarray(0, take));
      this.pending = this.pending.subarray(take);
      written = take;
    }
    while (written < out.length) {
      if (this.queue.queuedSamples === 0) {
        // The resampler holds `half` samples of group delay; push zeros so the
        // tail of the utterance emerges instead of being clipped to silence.
        if (this.draining > 0) {
          const zeros = new Float32Array(Math.min(this.draining, Math.ceil((out.length - written) * this.ratio) + 1));
          this.draining -= zeros.length;
          const resampled = this.resampler.push(zeros);
          const take = Math.min(out.length - written, resampled.length);
          out.set(resampled.subarray(0, take), written);
          written += take;
          if (take < resampled.length) this.pending = resampled.slice(take);
          continue;
        }
        out.fill(0, written);
        this.resampler.reset();
        this.setActive(false);
        return true;
      }
      this.draining = this.resampler.half;
      const need = (out.length - written) * this.ratio + this.carry;
      const whole = Math.max(1, Math.floor(need));
      this.carry = need - whole;
      if (this.scratch.length < whole) this.scratch = new Float32Array(whole);
      const source = this.scratch.subarray(0, whole);
      const real = this.queue.pull(source);
      const resampled = this.resampler.push(source.subarray(0, real));
      const take = Math.min(out.length - written, resampled.length);
      out.set(resampled.subarray(0, take), written);
      written += take;
      if (take < resampled.length) this.pending = resampled.slice(take);
      this.setActive(true);
    }
    if (written < out.length) out.fill(0, written);
    return true;
  }
}

registerProcessor("talker-playback", TalkerPlayback);
