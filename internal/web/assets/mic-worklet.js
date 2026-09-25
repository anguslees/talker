import { StreamingResampler, PCM16Framer, MIC_CREDITS } from "./audio.js";

class TalkerMicrophone extends AudioWorkletProcessor {
  constructor() {
    super();
    this.resampler = new StreamingResampler(sampleRate);
    this.framer = new PCM16Framer();
    this.enabled = false;
    this.epoch = 0;
    this.credits = MIC_CREDITS;
    this.speaking = false;
    this.quietFrames = 0;
    this.port.onmessage = ({ data }) => {
      if (data.type === "configure") {
        this.enabled = data.enabled;
        this.epoch = data.epoch;
        this.credits = MIC_CREDITS;
        this.speaking = false;
        this.quietFrames = 0;
        this.resampler.reset();
        this.framer.reset();
      } else if (data.type === "credit" && data.epoch === this.epoch) {
        this.credits = Math.min(MIC_CREDITS, this.credits + 1);
      }
    };
  }

  process(inputs) {
    const channels = inputs[0];
    if (!this.enabled || !channels?.length || !channels[0].length) return true;
    let mono = channels[0];
    if (channels.length > 1) {
      mono = new Float32Array(channels[0].length);
      for (const channel of channels) {
        for (let i = 0; i < mono.length; i++) mono[i] += channel[i] / channels.length;
      }
    }
    for (const frame of this.framer.push(this.resampler.push(mono))) {
      if (this.credits === 0) {
        this.enabled = false;
        this.framer.reset();
        this.port.postMessage({ type: "overflow", epoch: this.epoch });
        break;
      }
      // Activity only delays quiet-time notifications; interruption comes from the server.
      if (frame.rms >= (this.speaking ? 0.008 : 0.014)) {
        this.speaking = true;
        this.quietFrames = 0;
      } else if (++this.quietFrames >= 12) {
        this.speaking = false;
      }
      this.credits--;
      this.port.postMessage({ type: "frame", pcm: frame.pcm, speaking: this.speaking, epoch: this.epoch }, [frame.pcm]);
    }
    return true;
  }
}

registerProcessor("talker-microphone", TalkerMicrophone);
