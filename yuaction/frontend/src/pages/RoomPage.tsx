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
import { useMessages } from "../i18n";
import { LocaleSwitch } from "../i18n/LocaleSwitch";

export default function RoomPage({
  code,
  hostKey = "",
  isHost = false,
}: {
  code: string;
  hostKey?: string;
  isHost?: boolean;
}) {
  const m = useMessages();
  const { room, connection, error: loadError, accept } = useRoom(code);
  const { config } = useConfig();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [content, setContent] = useState("");
  const [quote, setQuote] = useState<Segment | null>(null);
  const [filter, setFilter] = useState("all");
  const [notice, setNotice] = useState("");
  const [demoText, setDemoText] = useState(m.room.demoSample);
  const [demoTranslation, setDemoTranslation] = useState(
    m.room.demoSampleTranslation,
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
    if (
      await mutate("/questions", {
        content,
        segmentId: quote?.segmentIds?.[0] || quote?.id || "",
        segmentIds: quote?.segmentIds,
      })
    ) {
      setContent("");
      setQuote(null);
      setNotice(m.room.questionSent);
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
      setNotice(m.room.demoSent);
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
        <LocaleSwitch />
        <Brand />
        <div className="panel loading-panel">
          <ErrorNote message={loadError} />
          {!loadError && <p>{m.room.loading}</p>}
          <a href="/">{m.room.backHome}</a>
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
          <LocaleSwitch />
          <Connection state={connection} />
          <a href="/" className="text-link">
            <ArrowLeft size={15} />
            {m.room.back}
          </a>
        </div>
      </header>
      <main className="room-main">
        <div className="room-heading">
          <div>
            <div className="eyebrow">
              {host ? m.room.hostEyebrow : m.room.guestEyebrow}
            </div>
            <h1>{room.title}</h1>
            <div className="room-subtitle">
              <Pill tone={room.status === "live" ? "green" : "neutral"}>
                <span className="tiny-dot" />
                {room.status === "live" ? m.room.live : m.room.ended}
              </Pill>
              <span>
                {room.kind === "classroom" ? m.room.classroom : m.room.talk}
              </span>
              <span>{m.room.room(room.code)}</span>
            </div>
          </div>
          {host && (
            <div className="heading-actions">
              <Button
                className="primary"
                onClick={() => inviteDialog.current?.showModal()}
              >
                <Users size={17} />
                {m.room.invite}
              </Button>
              <a
                className="button soft"
                href={`/rooms/${code}/display`}
                target="_blank"
                rel="noreferrer"
              >
                <Monitor size={17} />
                {m.room.display}
              </a>
              <Button
                className={room.status === "live" ? "outline" : "primary"}
                disabled={busy}
                onClick={() => {
                  if (
                    room.status !== "live" ||
                    window.confirm(m.room.endConfirm)
                  )
                    void mutate(
                      "",
                      { status: room.status === "live" ? "ended" : "live" },
                      "PATCH",
                    );
                }}
              >
                {room.status === "live" ? m.room.end : m.room.reopen}
              </Button>
            </div>
          )}
        </div>
        <nav className="room-shortcuts" aria-label={m.room.shortcuts}>
          {host && config?.yufoloConnected && (
            <a href="#transcription">
              <AudioLines size={15} />
              {m.room.transcription}
            </a>
          )}
          <a href="#captions">
            <AudioLines size={15} />
            {m.room.captions}
          </a>
          {!host && room.status === "live" && (
            <a href="#ask">
              <Send size={15} />
              {m.room.ask}
            </a>
          )}
          <a href="#questions">
            <MessageCircle size={15} />
            {host ? m.room.hostQuestions : m.room.guestQuestions}
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
                {m.room.received}
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
                {m.room.waiting}
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
                {m.room.answered}
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
              {m.room.discussing}
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
                  <h3>{m.room.composeTitle}</h3>
                  <MessageCircle size={19} />
                </div>
                <form onSubmit={submitQuestion}>
                  {quote && (
                    <div className="quote-preview">
                      <span>{m.room.quote(quote.text)}</span>
                      <button
                        type="button"
                        className="icon-button"
                        aria-label={m.room.clearQuote}
                        onClick={() => setQuote(null)}
                      >
                        <X size={16} />
                      </button>
                    </div>
                  )}
                  <textarea
                    ref={questionInput}
                    aria-label={m.room.questionLabel}
                    placeholder={m.room.questionPlaceholder}
                    value={content}
                    onChange={(e) => setContent(e.target.value)}
                    required
                    maxLength={1000}
                    disabled={room.status !== "live"}
                  />
                  <div className="compose-footer">
                    <small>{m.room.anonymous}</small>
                    <Button
                      type="submit"
                      className="primary"
                      disabled={
                        busy || room.status !== "live" || !content.trim()
                      }
                    >
                      <Send size={16} />
                      {room.status === "live" ? m.room.send : m.room.closed}
                    </Button>
                  </div>
                </form>
              </section>
            )}
            <section className="panel questions-panel" id="questions">
              <div className="panel-heading">
                <h3>
                  {host ? m.room.hostQuestions : m.room.guestQuestions}{" "}
                  <span className="count">{room.questions.length}</span>
                </h3>
                <MessageCircle size={18} />
              </div>
              <div className="tabs">
                {[
                  ["all", m.room.all],
                  ["pending", m.room.pending],
                  ["showing", m.room.showing],
                  ["answered", m.room.answeredTab],
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
                        if (confirm(m.room.deleteConfirm))
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
                    filter === "all" ? m.room.emptyAll : m.room.emptyFiltered
                  }
                >
                  {host ? m.room.emptyHost : m.room.emptyGuest}
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
                <h3>{m.room.integrationTitle}</h3>
                <p>
                  {config?.yufoloConnected
                    ? m.room.integrationLinked
                    : m.room.integrationWaiting}
                </p>
                <Pill
                  tone={
                    room.transcription === "recording" ? "green" : "neutral"
                  }
                >
                  {room.transcription === "recording"
                    ? m.room.recording
                    : room.transcription === "error" ||
                        room.transcription === "interrupted"
                      ? m.room.interrupted
                      : config?.yufoloConnected
                        ? m.room.waitingHost
                        : m.room.notConnected}
                </Pill>
              </section>
            )}
            {!host && <Share room={room} />}
            {host && config?.demo && (
              <section className="panel demo-panel">
                <div className="panel-heading">
                  <h3>{m.room.demoTitle}</h3>
                  <Pill tone="neutral">{m.room.demoOnly}</Pill>
                </div>
                <p className="form-note">{m.room.demoBody}</p>
                <form onSubmit={sendDemo}>
                  <label>
                    {m.room.demoOriginal}
                    <textarea
                      aria-label={m.room.demoOriginalLabel}
                      value={demoText}
                      onChange={(e) => setDemoText(e.target.value)}
                      required
                      maxLength={2000}
                    />
                  </label>
                  <label>
                    {m.room.demoTranslation}
                    <textarea
                      aria-label={m.room.demoTranslationLabel}
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
                    {m.room.demoSend}
                  </Button>
                </form>
              </section>
            )}
            {host && !config?.yufoloConnected && (
              <details className="panel key-panel">
                <summary>
                  <Settings2 size={16} />
                  {m.room.hostKey}
                </summary>
                <p>{m.room.hostKeyBody}</p>
                <input
                  type="password"
                  aria-label={m.room.hostKeyLabel}
                  readOnly
                  value={hostKey}
                  onFocus={(e) => e.target.select()}
                />
                <Button
                  className="soft small"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(hostKey);
                      setNotice(m.room.copied);
                    } catch {
                      setError(m.room.copyFailed);
                    }
                  }}
                >
                  <Copy size={14} />
                  {m.room.copy}
                </Button>
              </details>
            )}
            <p className="aside-signature">
              {m.room.signature}
              <br />
              <span>YUAction · BY COYUME</span>
            </p>
          </aside>
        </div>
        <footer>
          <span>YuAction · {m.room.footer}</span>
          <span>{host ? m.room.hostSpace : m.room.guestSpace}</span>
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
              <h2 id="invite-activity-title">{m.room.inviteTitle}</h2>
            </div>
            <button
              className="icon-button"
              aria-label={m.room.closeInvite}
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
