import { useRef, useState, type FormEvent } from "react";
import {
  ArrowLeft,
  AudioLines,
  Check,
  CircleHelp,
  Copy,
  MessageCircle,
  Monitor,
  Radio,
  Send,
  Settings2,
  Users,
  X,
} from "lucide-react";
import { api, type Room, type Segment } from "../api";
import { useRoom } from "../useRoom";
import { useConfig } from "../hooks/useConfig";
import {
  Brand,
  Button,
  Connection,
  Empty,
  ErrorNote,
  Pill,
} from "../components/ui";
import Captions from "../components/Captions";
import Share from "../components/Share";
import QuestionCard from "../components/QuestionCard";
import TranscriptionPanel from "../components/TranscriptionPanel";
import AssistantPanel from "../components/AssistantPanel";
import { useAssistant } from "../useAssistant";

export default function RoomPage({
  code,
  hostKey = "",
  isHost = false,
}: {
  code: string;
  hostKey?: string;
  isHost?: boolean;
}) {
  const { room, connection, error: loadError, accept } = useRoom(code);
  const { config } = useConfig();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [content, setContent] = useState("");
  const [quote, setQuote] = useState<Segment | null>(null);
  const [filter, setFilter] = useState("all");
  const [notice, setNotice] = useState("");
  const [demoText, setDemoText] = useState(
    "欢迎来到今天的课堂。任何时候有疑问，都可以在这里提出来。",
  );
  const [demoTranslation, setDemoTranslation] = useState(
    "Welcome to today’s class. Feel free to ask a question at any time.",
  );
  const questionInput = useRef<HTMLTextAreaElement>(null);
  const inviteDialog = useRef<HTMLDialogElement>(null);
  const host = isHost || !!hostKey;
  const assistant = useAssistant(code, hostKey, host);
  async function mutate(path: string, body: unknown, method = "POST") {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const next = await api<Room>(`/rooms/${code}${path}`, {
        method,
        body,
        key: hostKey,
        signal: AbortSignal.timeout(45000),
      });
      accept(next);
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      setBusy(false);
    }
  }
  async function submitQuestion(e: FormEvent) {
    e.preventDefault();
    if (await mutate("/questions", { content, segmentId: quote?.id || "" })) {
      setContent("");
      setQuote(null);
      setNotice("问题已发送，主持人和房间参与者都可以看到。");
    }
  }
  async function sendDemo(e: FormEvent) {
    e.preventDefault();
    if (
      await mutate("/demo-segments", {
        id: `demo-${crypto.randomUUID()}`,
        text: demoText,
        translation: demoTranslation,
      })
    )
      setNotice("演示字幕已同步到房间。未采集音频，也未调用转录服务。");
  }
  function quoteSegment(segment: Segment) {
    setQuote(segment);
    questionInput.current?.focus();
    questionInput.current?.scrollIntoView({
      behavior: "smooth",
      block: "center",
    });
  }
  if (!room)
    return (
      <div className="center-page">
        <Brand />
        <div className="panel loading-panel">
          <ErrorNote message={loadError} />
          {!loadError && <p>正在进入房间…</p>}
          <a href="/">返回活动空间</a>
        </div>
      </div>
    );
  const questions = room.questions
    .filter((q) => filter === "all" || q.status === filter)
    .slice()
    .reverse();
  const showing = room.questions.find((q) => q.status === "showing");
  return (
    <div className={`room-page ${host ? "host-page" : "participant-page"}`}>
      <header className="room-topbar">
        <Brand />
        <div className="room-topbar-right">
          <Connection state={connection} />
          <a href="/" className="text-link">
            <ArrowLeft size={15} />
            活动空间
          </a>
        </div>
      </header>
      <main className="room-main">
        <div className="room-heading">
          <div>
            <div className="eyebrow">
              {host
                ? "主持人工作台 / LIVE WORKSPACE"
                : "一起参与 / LIVE SESSION"}
            </div>
            <h1>{room.title}</h1>
            <div className="room-subtitle">
              <Pill tone={room.status === "live" ? "green" : "neutral"}>
                <span className="tiny-dot" />
                {room.status === "live" ? "活动进行中" : "活动已结束"}
              </Pill>
              <span>{room.kind === "classroom" ? "互动课堂" : "现场演讲"}</span>
              <span>房间 {room.code}</span>
            </div>
          </div>
          {host && (
            <div className="heading-actions">
              <Button
                className="primary"
                onClick={() => inviteDialog.current?.showModal()}
              >
                <Users size={17} />
                邀请参与
              </Button>
              <a
                className="button soft"
                href={`/rooms/${code}/display`}
                target="_blank"
                rel="noreferrer"
              >
                <Monitor size={17} />
                打开大屏
              </a>
              <Button
                className={room.status === "live" ? "outline" : "primary"}
                disabled={busy}
                onClick={() => {
                  if (
                    room.status !== "live" ||
                    window.confirm(
                      "结束后将停止接收新问题和字幕，已有内容仍可查看。确定结束？",
                    )
                  )
                    void mutate(
                      "",
                      { status: room.status === "live" ? "ended" : "live" },
                      "PATCH",
                    );
                }}
              >
                {room.status === "live" ? "结束活动" : "重新开启"}
              </Button>
            </div>
          )}
        </div>
        <nav className="room-shortcuts" aria-label="活动内容">
          {host && config?.yufoloConnected && (
            <a href="#transcription">
              <AudioLines size={15} />
              转录控制
            </a>
          )}
          <a href="#captions">
            <AudioLines size={15} />
            共享字幕
          </a>
          {!host && room.status === "live" && (
            <a href="#ask">
              <Send size={15} />
              我要提问
            </a>
          )}
          <a href="#questions">
            <MessageCircle size={15} />
            {host ? "现场提问" : "大家的提问"}
            <span>{room.questions.length}</span>
          </a>
        </nav>
        <ErrorNote message={loadError || error} />
        {notice && (
          <div className="notice" role="status">
            <Check size={17} />
            {notice}
          </div>
        )}
        {host && (
          <div className="metrics-row">
            <div>
              <span className="metric-icon">
                <MessageCircle size={20} />
              </span>
              <span>
                收到的问题
                <strong>
                  {room.questions.length.toString().padStart(2, "0")}
                </strong>
              </span>
            </div>
            <div>
              <span className="metric-icon lavender">
                <CircleHelp size={20} />
              </span>
              <span>
                等待回应
                <strong>
                  {room.questions
                    .filter((q) => q.status === "pending")
                    .length.toString()
                    .padStart(2, "0")}
                </strong>
              </span>
            </div>
            <div>
              <span className="metric-icon green">
                <Check size={20} />
              </span>
              <span>
                已经解答
                <strong>
                  {room.questions
                    .filter((q) => q.status === "answered")
                    .length.toString()
                    .padStart(2, "0")}
                </strong>
              </span>
            </div>
          </div>
        )}
        {!host && showing && (
          <section className="spotlight">
            <span>
              <Radio size={16} />
              现在正在讨论
            </span>
            <h2>{showing.content}</h2>
          </section>
        )}
        <div className="room-grid">
          <div className="room-primary">
            {host && (
              <AssistantPanel
                code={code}
                hostKey={hostKey}
                info={assistant.info}
                error={assistant.error}
                refresh={assistant.refresh}
              />
            )}
            {!host && (
              <section className="panel question-compose" id="ask">
                <div className="panel-heading">
                  <h3>你的问题，值得被听见。</h3>
                  <MessageCircle size={19} />
                </div>
                <form onSubmit={submitQuestion}>
                  {quote && (
                    <div className="quote-preview">
                      <span>关于这段字幕：{quote.text}</span>
                      <button
                        type="button"
                        className="icon-button"
                        aria-label="取消引用"
                        onClick={() => setQuote(null)}
                      >
                        <X size={16} />
                      </button>
                    </div>
                  )}
                  <textarea
                    ref={questionInput}
                    aria-label="你的问题"
                    placeholder="有什么疑问，或者想进一步了解的地方？"
                    value={content}
                    onChange={(e) => setContent(e.target.value)}
                    required
                    maxLength={1000}
                    disabled={room.status !== "live"}
                  />
                  <div className="compose-footer">
                    <small>匿名提问 · 房间内公开</small>
                    <Button
                      type="submit"
                      className="primary"
                      disabled={
                        busy || room.status !== "live" || !content.trim()
                      }
                    >
                      <Send size={16} />
                      {room.status === "live" ? "发送问题" : "活动已结束"}
                    </Button>
                  </div>
                </form>
              </section>
            )}
            <section className="panel questions-panel" id="questions">
              <div className="panel-heading">
                <h3>
                  {host ? "现场提问" : "大家的提问"}{" "}
                  <span className="count">{room.questions.length}</span>
                </h3>
                <MessageCircle size={18} />
              </div>
              <div className="tabs">
                {[
                  ["all", "全部"],
                  ["pending", "待回应"],
                  ["showing", "正在展示"],
                  ["answered", "已解答"],
                ].map(([value, label]) => (
                  <button
                    key={value}
                    className={filter === value ? "selected" : ""}
                    aria-pressed={filter === value}
                    onClick={() => setFilter(value)}
                  >
                    {label}
                  </button>
                ))}
              </div>
              {questions.length ? (
                <div className="question-list">
                  {questions.map((q) => (
                    <QuestionCard
                      question={q}
                      key={q.id}
                      host={host}
                      disabled={busy || room.status !== "live"}
                      aiEnabled={assistant.info?.configured}
                      draft={assistant.info?.answers.find(
                        (a) => a.questionId === q.id,
                      )}
                      onGenerate={() => void assistant.generate(q.id)}
                      onDelete={() => {
                        if (confirm("删除这个问题及其 AI 草稿？"))
                          void mutate(
                            `/questions/${q.id}`,
                            undefined,
                            "DELETE",
                          );
                      }}
                      onStatus={(status) =>
                        void mutate(`/questions/${q.id}`, { status }, "PATCH")
                      }
                    />
                  ))}
                </div>
              ) : (
                <Empty
                  title={
                    filter === "all"
                      ? "给第一个问题一点时间"
                      : "这里暂时没有问题"
                  }
                >
                  {host
                    ? "分享房间码，让台下的想法来到这里。"
                    : "关于刚才的内容，你有什么想问的吗？"}
                </Empty>
              )}
            </section>
            <Captions
              code={code}
              chooseTranslation={!host && !!config?.yufoloConnected}
              segments={room.segments}
              onQuote={
                host || room.status !== "live" ? undefined : quoteSegment
              }
            />
          </div>
          <aside className="room-aside">
            {host && config?.yufoloConnected ? (
              <TranscriptionPanel room={room} hostKey={hostKey} />
            ) : (
              <section className="panel integration-panel">
                <span className="integration-label">
                  <AudioLines size={18} />
                  YUFOLO CONNECTION
                </span>
                <h3>让所有人，跟上同一段讲述。</h3>
                <p>
                  {config?.yufoloConnected
                    ? "主持人开启一次转录，大家同步阅读同一份字幕。"
                    : "共享转录尚未配置，连接 Yufolo 后即可开始。"}
                </p>
                <Pill
                  tone={
                    room.transcription === "recording" ? "green" : "neutral"
                  }
                >
                  {room.transcription === "recording"
                    ? "正在转录"
                    : room.transcription === "error" ||
                        room.transcription === "interrupted"
                      ? "转录已中断，等待主持人恢复"
                      : config?.yufoloConnected
                        ? "等待主持人开启转录"
                        : "真实转录尚未连接"}
                </Pill>
              </section>
            )}
            {!host && <Share room={room} />}
            {host && config?.demo && (
              <section className="panel demo-panel">
                <div className="panel-heading">
                  <h3>体验字幕同步</h3>
                  <Pill tone="neutral">仅演示</Pill>
                </div>
                <p className="form-note">
                  输入一句话，查看其他参与者和大屏的同步效果。
                </p>
                <form onSubmit={sendDemo}>
                  <label>
                    原文
                    <textarea
                      aria-label="演示字幕原文"
                      value={demoText}
                      onChange={(e) => setDemoText(e.target.value)}
                      required
                      maxLength={2000}
                    />
                  </label>
                  <label>
                    译文（可选）
                    <textarea
                      aria-label="演示字幕译文"
                      value={demoTranslation}
                      onChange={(e) => setDemoTranslation(e.target.value)}
                      maxLength={2000}
                    />
                  </label>
                  <Button
                    type="submit"
                    className="soft full"
                    disabled={busy || room.status !== "live"}
                  >
                    <Radio size={16} />
                    发送演示字幕
                  </Button>
                </form>
              </section>
            )}
            {host && !config?.yufoloConnected && (
              <details className="panel key-panel">
                <summary>
                  <Settings2 size={16} />
                  主持人访问密钥
                </summary>
                <p>
                  此标签页会记住密钥。请妥善保存，以便关闭标签页后重新管理活动；不要分享给参与者。
                </p>
                <input
                  type="password"
                  aria-label="主持人访问密钥"
                  readOnly
                  value={hostKey}
                  onFocus={(e) => e.target.select()}
                />
                <Button
                  className="soft small"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(hostKey);
                      setNotice("主持人密钥已复制，请保存在安全的地方。");
                    } catch {
                      setError("无法使用剪贴板，请选中密钥后手动复制。");
                    }
                  }}
                >
                  <Copy size={14} />
                  复制密钥
                </Button>
              </details>
            )}
            <p className="aside-signature">
              少一点距离，多一点回应。
              <br />
              <span>YUAction · BY COYUME</span>
            </p>
          </aside>
        </div>
        <footer>
          <span>YuAction · 每个声音都有位置</span>
          <span>{host ? "主持人工作台" : "参与者空间"}</span>
        </footer>
      </main>
      {host && (
        <dialog
          ref={inviteDialog}
          className="create-dialog invite-dialog"
          aria-labelledby="invite-activity-title"
        >
          <div className="dialog-heading">
            <div>
              <span className="eyebrow">INVITE YOUR AUDIENCE</span>
              <h2 id="invite-activity-title">分享这个现场</h2>
            </div>
            <button
              className="icon-button"
              aria-label="关闭邀请"
              onClick={() => inviteDialog.current?.close()}
            >
              <X size={20} />
            </button>
          </div>
          <Share room={room} />
        </dialog>
      )}
    </div>
  );
}
