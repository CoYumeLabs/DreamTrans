import { useEffect, useRef, useState } from "react";
import { api, type Room } from "../api";
import { RecordingTransport } from "../RecordingTransport";
import { Button, ErrorNote, Pill } from "./ui";
import { AudioLines, Mic, Pause } from "lucide-react";
import { useMessages } from "../i18n";

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
export default function TranscriptionPanel({
  room,
  hostKey,
}: {
  room: Room;
  hostKey: string;
}) {
  const m = useMessages();
  const languages = [
    ["cmn_en", m.transcription.auto],
    ["cmn", m.transcription.cmn],
    ["en", m.transcription.en],
    ["ja", m.transcription.ja],
    ["ko", m.transcription.ko],
    ["de", m.transcription.de],
    ["fr", m.transcription.fr],
    ["es", m.transcription.es],
  ];
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
      .catch((e) => setError((e as Error).message));
    const cancel = () => {
      generation.current++;
      cleanup();
    };
    // A hide event before the microphone is open must not abandon the start
    // while the button still says it is connecting.
    const onHide = () => {
      if (capture.current.stream || capture.current.transport) cancel();
    };
    window.addEventListener("pagehide", onHide);
    return () => {
      window.removeEventListener("pagehide", onHide);
      cancel();
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
        throw new Error(m.transcription.insecure);
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          channelCount: 1,
          echoCancellation: true,
          noiseSuppression: true,
        },
      });
      if (generation.current !== own) {
        stream.getTracks().forEach((t) => t.stop());
        setState("idle");
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
        setState("idle");
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
              track.onended = () => fail(m.transcription.micEnded);
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
          <h3>{m.transcription.title}</h3>
        </div>
        <AudioLines className="control-wave" size={28} />
      </div>
      <Pill tone={state === "recording" ? "green" : "neutral"}>
        {state === "recording"
          ? m.transcription.recording
          : state === "starting"
            ? m.transcription.starting
            : state === "stopping"
              ? m.transcription.stopping
              : recordingElsewhere
                ? m.transcription.elsewhere
                : m.transcription.idle}
      </Pill>
      <div className="language-fields">
        <label>
          {m.transcription.source}
          <select
            aria-label={m.transcription.sourceLabel}
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
      <p>{m.transcription.body}</p>
      {source === "cmn_en" && (
        <p className="form-note">{m.transcription.autoHint}</p>
      )}
      <ErrorNote message={error} />
      {state === "recording" ? (
        <Button className="outline full" onClick={() => void stop()}>
          <Pause size={17} />
          {m.transcription.pause}
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
          {m.transcription.cancel}
        </Button>
      ) : (
        <Button
          className="primary full"
          disabled={working || recordingElsewhere || room.status !== "live"}
          onClick={() => void start()}
        >
          <Mic size={17} />
          {working
            ? m.transcription.wait
            : recordingElsewhere
              ? m.transcription.receiving
              : room.status !== "live"
                ? m.transcription.ended
                : m.transcription.start}
        </Button>
      )}
      {link?.linked && <p className="form-note">{m.transcription.linked}</p>}
    </section>
  );
}
