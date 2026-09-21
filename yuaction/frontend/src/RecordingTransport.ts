// The microphone belongs to the capture session, never to a deployment socket.
// A planned handoff has an explicit commit boundary: all old audio is finalized
// before `migrated`; audio captured after our handoff command goes only to the
// successor. An ambiguous network failure is surfaced, never silently replayed.
export class RecordingTransport {
  private socket?: WebSocket;
  private queue: ArrayBuffer[] = [];
  private bytes = 0;
  private phase: "connecting" | "ready" | "handoff" | "ending" | "closed" =
    "connecting";
  private stopping = false;
  private preflighting = false;
  private retry = 0;
  private timer?: ReturnType<typeof setTimeout>;
  private pump: ReturnType<typeof setInterval>;
  private started = false;

  constructor(
    private options: {
      url: string;
      sampleRate: number;
      preflight: () => Promise<unknown>;
      ready: () => void;
      stopped: () => void;
      error: (message: string) => void;
    },
  ) {
    this.pump = setInterval(() => this.drain(), 20);
    this.connect();
  }

  private connect() {
    if (this.phase === "closed") return;
    this.phase = "connecting";
    const socket = new WebSocket(this.options.url);
    this.socket = socket;
    let ready = false;
    this.deadline(30000, "转录连接超时，请检查网络和账号状态。");
    socket.onmessage = (event) => {
      if (this.socket !== socket || this.phase === "closed") return;
      let message: { type?: string; version?: number; message?: string };
      try {
        message = JSON.parse(event.data);
      } catch {
        this.fail("转录响应格式不正确。");
        return;
      }
      if (message.type === "ready") {
        clearTimeout(this.timer);
        ready = true;
        this.retry = 0;
        this.phase = "ready";
        if (!this.started) {
          this.started = true;
          this.options.ready();
        }
        this.drain();
      } else if (message.type === "handoff" && message.version === 1) {
        void this.handoff();
      } else if (
        message.type === "migrated" &&
        message.version === 1 &&
        this.phase === "handoff"
      ) {
        clearTimeout(this.timer);
        this.socket = undefined;
        socket.close();
        this.connect();
      } else if (message.type === "stopped") {
        this.close();
        this.options.stopped();
      } else if (message.type === "error") {
        this.fail(message.message || "转录中断，请检查连接。");
      }
    };
    // A successor may briefly wait for main's previous paid session to close.
    // Retry only before RecognitionStarted: no queued audio has been sent yet.
    socket.onerror = () => {
      /* onclose decides whether a retry is safe */
    };
    socket.onclose = () => {
      if (this.socket !== socket || this.phase === "closed") return;
      clearTimeout(this.timer);
      if (!ready && this.retry++ < 20) {
        this.timer = setTimeout(
          () => this.connect(),
          Math.min(250 * this.retry, 1500),
        );
      } else {
        this.fail("转录连接意外断开，请检查网络；已确认字幕已保留。");
      }
    };
  }

  send(data: ArrayBuffer) {
    if (this.phase === "closed" || this.stopping) return;
    if (this.bytes + data.byteLength > this.options.sampleRate * 2 * 60) {
      this.fail("连接恢复超过音频缓存容量，录音已暂停，请检查网络。");
      return;
    }
    this.queue.push(data);
    this.bytes += data.byteLength;
    this.drain();
  }

  private drain() {
    const socket = this.socket;
    if (this.phase !== "ready" || socket?.readyState !== WebSocket.OPEN) return;
    while (this.queue.length && socket.bufferedAmount < 256 * 1024) {
      const frame = this.queue.shift()!;
      this.bytes -= frame.byteLength;
      socket.send(frame);
    }
    if (this.stopping && !this.queue.length) {
      this.phase = "ending";
      socket.send(JSON.stringify({ type: "stop" }));
      this.deadline(30000, "最后一段字幕保存超时，请检查连接。");
    }
  }

  private async handoff() {
    if (this.preflighting || this.phase !== "ready" || this.stopping) return;
    this.preflighting = true;
    const socket = this.socket;
    try {
      // Verify the serving route/auth before ending a healthy connection. A
      // failed candidate leaves capture and the existing socket untouched.
      await this.options.preflight();
      if (this.socket !== socket || this.phase !== "ready" || this.stopping)
        return;
      this.phase = "handoff";
      socket!.send(JSON.stringify({ type: "handoff" }));
      this.deadline(40000, "录音交接超时，请检查网络；已确认字幕已保留。");
    } catch {
      // The deployment controller can offer again after readiness recovers.
    } finally {
      this.preflighting = false;
    }
  }

  stop() {
    this.stopping = true;
    this.drain(); // If handing off, the successor first sends all buffered audio.
  }

  close() {
    this.phase = "closed";
    clearTimeout(this.timer);
    clearInterval(this.pump);
    this.socket?.close();
    this.socket = undefined;
    this.queue = [];
    this.bytes = 0;
  }

  private deadline(ms: number, message: string) {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.fail(message), ms);
  }
  private fail(message: string) {
    if (this.phase === "closed") return;
    this.close();
    this.options.error(message);
  }
}
