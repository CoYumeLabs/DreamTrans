import { useEffect, useRef, useState } from "react";
import { api, type Room } from "../api";
import { RecordingTransport } from "../RecordingTransport";
import { Button, ErrorNote, Pill } from "./ui";
import { AudioLines, Mic, Pause } from "lucide-react";

type Link = {
  sessionId: string;
  sourceLanguage: string;
  targetLanguage: string;
  active: boolean;
  linked: boolean;
};
type Capture = {
  stream?: MediaStream;
  context?: AudioContext;
  worklet?: AudioWorkletNode;
  transport?: RecordingTransport;
  flush?: () => void;
  timer?: ReturnType<typeof setTimeout>;
};
const languages = [
  ["cmn_en", "自动（中英混合）"],
  ["cmn", "中文"],
  ["en", "英语"],
  ["ja", "日语"],
  ["ko", "韩语"],
  ["de", "德语"],
  ["fr", "法语"],
  ["es", "西班牙语"],
];
export default function TranscriptionPanel({
  room,
  hostKey,
}: {
  room: Room;
  hostKey: string;
}) {
  const [source, setSource] = useState("cmn");

  const [link, setLink] = useState<Link | null>(null);
  const [state, setState] = useState("idle");
  const [error, setError] = useState("");
  const capture = useRef<Capture>({});
  const generation = useRef(0);
  function releaseMic(c: Capture) {
    c.stream?.getTracks().forEach((t) => t.stop());
    c.worklet?.disconnect();
    if (c.context && c.context.state !== "closed") void c.context.close();
    c.stream = undefined;
    c.context = undefined;
  }
  function cleanup() {
    const c = capture.current;
    clearTimeout(c.timer);
    releaseMic(c);
    c.transport?.close();
    capture.current = {};
  }
  useEffect(() => {
    api<Link>(`/rooms/${room.code}/transcription`, { key: hostKey })
      .then((v) => {
        setLink(v);
        if (v.sourceLanguage) {
          setSource(v.sourceLanguage);
        }
      })
      .catch((e) => setError(e.message));
    const leave = () => {
      generation.current++;
      cleanup();
    };
    window.addEventListener("pagehide", leave);
    return () => {
      window.removeEventListener("pagehide", leave);
      leave();
    };
  }, [room.code]);
  useEffect(() => {
    if (room.status === "ended") {
      generation.current++;
      cleanup();
      setState("idle");
    }
  }, [room.status]);
  async function start() {
    setError("");
    setState("starting");
    const own = ++generation.current;
    try {
      if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia)
        throw new Error(
          "麦克风需要 HTTPS 或 localhost，请使用安全地址打开主持人页面。",
        );
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
        },
      });
      if (generation.current !== own) {
        stream.getTracks().forEach((t) => t.stop());
        return;
      }
      const c: Capture = { stream };
      capture.current = c;
      const context = new AudioContext();
      c.context = context;
      await context.resume();
      await context.audioWorklet.addModule("/yuaction-pcm.js");
      const next = await api<Link>(`/rooms/${room.code}/transcription`, {
        method: "POST",
        key: hostKey,
        body: { sourceLanguage: source, targetLanguage: "" },
      });
      if (generation.current !== own) {
        releaseMic(c);
        return;
      }
      setLink(next);
      const fail = (message: string) => {
        if (generation.current !== own) return;
        generation.current++;
        cleanup();
        setState("idle");
        setError(message);
      };
      c.transport = new RecordingTransport({
        url: `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/api/rooms/${room.code}/audio?sampleRate=${context.sampleRate}&protocol=1&capture=${crypto.randomUUID()}`,
        sampleRate: context.sampleRate,
        preflight: () =>
          api<Link>(`/rooms/${room.code}/transcription`, { key: hostKey }),
        error: fail,
        stopped: () => {
          if (generation.current !== own) return;
          generation.current++;
          cleanup();
          setState("idle");
        },
        ready: () => {
          if (generation.current !== own) return;
          try {
            const worklet = new AudioWorkletNode(context, "yuaction-pcm");
            c.worklet = worklet;
            worklet.port.onmessage = (
              event: MessageEvent<ArrayBuffer | string>,
            ) => {
              if (event.data === "flushed") {
                c.flush?.();
                return;
              }
              if (event.data instanceof ArrayBuffer)
                c.transport?.send(event.data);
            };
            const silent = context.createGain();
            silent.gain.value = 0;
            context
              .createMediaStreamSource(stream)
              .connect(worklet)
              .connect(silent)
              .connect(context.destination);
            stream.getTracks().forEach((track) => {
              track.onended = () => fail("麦克风已断开，请重新开始转录。");
            });
            setState("recording");
          } catch (e) {
            fail((e as Error).message);
          }
        },
      });
    } catch (e) {
      if (generation.current === own) {
        generation.current++;
        cleanup();
        setState("idle");
        setError((e as Error).message);
      }
    }
  }
  async function stop() {
    setState("stopping");
    const c = capture.current;
    if (c.worklet)
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, 500);
        c.flush = () => {
          clearTimeout(timer);
          resolve();
        };
        c.worklet!.port.postMessage("flush");
      });
    releaseMic(c);
    c.transport?.stop();
  }
  const working = state !== "idle";
  const recordingElsewhere =
    state === "idle" && room.transcription === "recording";
  return (
    <section
      id="transcription"
      className={`panel transcription-panel ${state === "recording" ? "is-recording" : ""}`}
    >
      <div className="transcription-heading">
        <span className="control-icon">
          <Mic size={22} />
        </span>
        <div>
          <span className="eyebrow">LIVE TRANSCRIPTION</span>
          <h3>共享实时转录</h3>
        </div>
        <AudioLines className="control-wave" size={28} />
      </div>
      <Pill tone={state === "recording" ? "green" : "neutral"}>
        {state === "recording"
          ? "正在采集麦克风"
          : state === "starting"
            ? "正在连接…"
            : state === "stopping"
              ? "正在保存最后的字幕…"
              : recordingElsewhere
                ? "其他主持端正在转录"
                : "麦克风未开启"}
      </Pill>
      <div className="language-fields">
        <label>
          原文语言
          <select
            aria-label="原文语言"
            value={source}
            disabled={working || !!link?.sessionId}
            onChange={(e) => {
              setSource(e.target.value);
            }}
          >
            {languages.map(([v, name]) => (
              <option key={v} value={v}>
                {name}
              </option>
            ))}
          </select>
        </label>
      </div>
      <p>
        仅此主持端采集音频，参与者自行选择译文语言；同一种译文共享生成。转录与
        AI 翻译使用你的 Yufolo 余额。
      </p>
      {source === "cmn_en" && (
        <p className="form-note">
          自动模式识别中文与英文混合讲话；其他语种请手动选择。
        </p>
      )}
      <ErrorNote message={error} />
      {state === "recording" ? (
        <Button className="outline full" onClick={() => void stop()}>
          <Pause size={17} />
          暂停转录
        </Button>
      ) : state === "starting" ? (
        <Button
          className="outline full"
          onClick={() => {
            generation.current++;
            cleanup();
            setState("idle");
          }}
        >
          取消连接
        </Button>
      ) : (
        <Button
          className="primary full"
          disabled={working || recordingElsewhere || room.status !== "live"}
          onClick={() => void start()}
        >
          <Mic size={17} />
          {working
            ? "请稍候…"
            : recordingElsewhere
              ? "正在接收共享字幕"
              : room.status !== "live"
                ? "活动已结束"
                : "开始转录"}
        </Button>
      )}
      {link?.linked && (
        <p className="form-note">
          已关联 Yufolo 会话，确认的字幕会同步保存到你的 Yufolo 账号。
        </p>
      )}
    </section>
  );
}
