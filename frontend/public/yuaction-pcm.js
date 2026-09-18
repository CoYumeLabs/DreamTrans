class YuActionPCM extends AudioWorkletProcessor {
  constructor() {
    super();
    this.samples = new Int16Array(2048);
    this.used = 0;
    this.port.onmessage = () => {
      this.flush();
      this.port.postMessage("flushed");
    };
  }
  flush() {
    if (!this.used) return;
    const bytes = new ArrayBuffer(this.used * 2);
    const view = new DataView(bytes);
    for (let i = 0; i < this.used; i++)
      view.setInt16(i * 2, this.samples[i], true);
    this.port.postMessage(bytes, [bytes]);
    this.used = 0;
  }
  process(inputs) {
    const channels = inputs[0];
    if (channels?.length)
      for (let i = 0; i < channels[0].length; i++) {
        let value = 0;
        for (const ch of channels) value += ch[i] / channels.length;
        value = Math.max(-1, Math.min(1, value));
        this.samples[this.used++] = value < 0 ? value * 32768 : value * 32767;
        if (this.used === this.samples.length) this.flush();
      }
    return true;
  }
}
registerProcessor("yuaction-pcm", YuActionPCM);
