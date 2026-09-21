import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  ArrowRight,
  AudioLines,
  ChevronRight,
  GraduationCap,
  LayoutDashboard,
  MessageCircle,
  Radio,
  ScanLine,
  Mic,
  Plus,
  Users,
  X,
} from "lucide-react";
import { api, recentRooms, rememberRoom, saveHostKey, type Room } from "../api";
import { useConfig } from "../hooks/useConfig";
import { Brand, Button, ErrorNote, Pill } from "../components/ui";
import AccountPanel, { type Account } from "../components/AccountPanel";
import { intlLocale, useMessages } from "../i18n";
import { LocaleSwitch } from "../i18n/LocaleSwitch";

export default function Home() {
  const m = useMessages();
  const { config, error: configError } = useConfig();
  const [creating, setCreating] = useState(false);
  const [kind, setKind] = useState<"classroom" | "talk">("classroom");
  const [title, setTitle] = useState("");
  const [creatorKey, setCreatorKey] = useState("");
  const [joinCode, setJoinCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [user, setUser] = useState<Account | null>(null);
  const [ownedRooms, setOwnedRooms] = useState<Room[]>([]);
  const [awaitingLogin, setAwaitingLogin] = useState(false);
  const accountSlot = useRef<HTMLDivElement>(null);
  const rooms = config?.yufoloConnected ? ownedRooms : recentRooms();
  useEffect(() => {
    if (!user) {
      setOwnedRooms([]);
      return;
    }
    api<Room[]>("/my/rooms")
      .then(setOwnedRooms)
      .catch((e) => setError(e.message));
  }, [user]);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (user && awaitingLogin) {
      setAwaitingLogin(false);
      setCreating(true);
    }
  }, [user, awaitingLogin]);
  function beginCreate() {
    setError("");
    if (config?.yufoloConnected && !user) {
      setAwaitingLogin(true);
      accountSlot.current?.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
      accountSlot.current
        ?.querySelector("input")
        ?.focus({ preventScroll: true });
      return;
    }
    setCreating(true);
  }
  useEffect(() => {
    if (creating) dialog.current?.showModal();
    else dialog.current?.close();
  }, [creating]);

  async function create(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const result = await api<{ room: Room; hostKey: string }>("/rooms", {
        method: "POST",
        body: { title, kind },
        key: creatorKey,
      });
      saveHostKey(result.room.code, result.hostKey);
      rememberRoom(result.room);
      window.location.assign(`/rooms/${result.room.code}/host`);
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  }
  function join(e: FormEvent) {
    e.preventDefault();
    const code = joinCode.trim().toUpperCase();
    if (!/^[A-F0-9]{8}$/.test(code)) {
      setError(m.home.badCode);
      return;
    }
    window.location.assign(`/rooms/${code}`);
  }
  return (
    <div className="app-shell">
      <aside className="sidebar">
        <Brand />
        <div className="workspace-label">{m.home.workspace}</div>
        <nav>
          <a className="nav-item active" href="/">
            <LayoutDashboard size={18} />
            {m.home.activities}
          </a>
          <a className="nav-item" href="#join">
            <Users size={18} />
            {m.home.joinNav}
          </a>
          <a
            className="nav-item"
            href="https://yufolo.com"
            target="_blank"
            rel="noreferrer"
          >
            <AudioLines size={18} />
            {m.home.yufoloSpace}
            <ArrowRight size={14} className="nav-arrow" />
          </a>
        </nav>
        <div className="sidebar-note">
          <span className="mini-label">{m.home.noteKicker}</span>
          <p>
            {m.home.noteBody}
            <br />
            {m.home.noteBodyLine}
          </p>
          <span>BY COYUME</span>
        </div>
        <div className="sidebar-bottom">
          <span className="avatar">Y</span>
          <div>
            <strong>{m.home.sidebarTitle}</strong>
            <small>{m.home.sidebarMeta}</small>
          </div>
        </div>
      </aside>
      <main className="home-main">
        <header className="topbar">
          <span className="workspace-breadcrumb">
            {m.home.workspace} <ChevronRight size={14} />
            <strong>{m.home.overview}</strong>
          </span>
          <div className="mobile-brand">
            <Brand />
          </div>
          <div className="topbar-tools">
            <span className="topbar-note">
              <Radio size={14} /> {m.home.topNote}
            </span>
            <LocaleSwitch />
          </div>
        </header>
        <div className="home-content">
          <div className="page-heading">
            <div>
              <div className="eyebrow">YOUR NEXT CONVERSATION</div>
              <h1>
                {m.home.headline}<span>{m.home.headlineMark}</span>
              </h1>
              <p>{m.home.lede}</p>
            </div>
            <div className="page-actions">
              <a href="#join" className="button outline">
                <Users size={16} />
                {m.home.join}
              </a>
              <Button
                className="primary"
                onClick={beginCreate}
                disabled={!config}
              >
                <Plus size={18} />
                {m.home.create}
              </Button>
            </div>
          </div>
          <div
            className={`welcome-grid ${config?.yufoloConnected ? "with-account" : ""} ${user ? "is-authenticated" : ""}`}
          >
            <section className="hero-card">
              <div className="hero-copy">
                <span className="hero-label">
                  <span className="tiny-dot" /> {m.home.heroKicker}
                </span>
                <h2>
                  {m.home.heroTitle}
                  <br />
                  <span>{m.home.heroTitleLine}</span>
                </h2>
                <p>
                  {m.home.heroBody}
                  <br />
                  {m.home.heroBodyLine}
                </p>
                <a href="#join" className="hero-link">
                  {m.home.heroLink} <ArrowRight size={16} />
                </a>
              </div>
              <div className="hero-visual" aria-hidden="true">
                <div className="signal-ring ring-outer" />
                <div className="signal-ring ring-inner" />
                <div className="signal-center">
                  <AudioLines size={38} strokeWidth={1.5} />
                </div>
                <span className="signal-chip chip-caption">
                  <AudioLines size={15} /> {m.home.chipCaption}
                </span>
                <span className="signal-chip chip-question">
                  <MessageCircle size={15} /> {m.home.chipQuestion}
                </span>
                <span className="signal-chip chip-people">
                  <Users size={15} /> {m.home.chipPeople}
                </span>
              </div>
              <div className="hero-steps">
                <span>
                  <ScanLine size={16} /> {m.home.stepScan}
                </span>
                <i />
                <span>
                  <AudioLines size={16} /> {m.home.stepCaptions}
                </span>
                <i />
                <span>
                  <MessageCircle size={16} /> {m.home.stepAsk}
                </span>
              </div>
            </section>
            {config?.yufoloConnected && (
              <div className="account-slot" ref={accountSlot}>
                {awaitingLogin && (
                  <p className="account-prompt" role="status">
                    {m.home.loginPrompt}
                  </p>
                )}
                <AccountPanel onChange={setUser} />
              </div>
            )}
          </div>
          <div className="section-title">
            <div>
              <h2>
                {m.home.myActivities}{" "}
                <span>{rooms.length.toString().padStart(2, "0")}</span>
              </h2>
              <p>
                {config?.yufoloConnected ? m.home.ownedHint : m.home.recentHint}
              </p>
            </div>
            <span className="muted-label">{m.home.returnHint}</span>
          </div>
          <div className={`activity-grid ${rooms.length ? "" : "is-empty"}`}>
            {rooms.map((room) => (
              <a
                className="activity-card"
                href={`/rooms/${room.code}/host`}
                key={room.code}
              >
                <div className="activity-card-header">
                  <span className={`activity-icon ${room.kind}`}>
                    {room.kind === "classroom" ? (
                      <GraduationCap size={24} />
                    ) : (
                      <Mic size={24} />
                    )}
                  </span>
                  {"status" in room && (
                    <Pill tone={room.status === "live" ? "green" : "neutral"}>
                      {room.status === "live" ? m.home.live : m.home.ended}
                    </Pill>
                  )}
                </div>
                <div className="activity-card-title">
                  <h3>{room.title}</h3>
                  <ArrowRight size={18} />
                </div>
                <p>
                  {room.kind === "classroom" ? m.home.classroom : m.home.talk} ·{" "}
                  {new Date(room.createdAt).toLocaleDateString(intlLocale())}
                </p>
                <div className="activity-bottom">
                  <span>{m.home.roomCode}</span>
                  <strong>{room.code}</strong>
                </div>
              </a>
            ))}
            <button
              className="new-activity"
              onClick={beginCreate}
              disabled={!config}
            >
              <span>
                <Plus size={24} />
              </span>
              <strong>{m.home.createAnother}</strong>
              <small>
                {rooms.length ? m.home.createAnotherHint : m.home.createFirstHint}
              </small>
            </button>
          </div>
          <div className="home-bottom-grid">
            <section className="join-card" id="join">
              <span className="small-icon">
                <Users size={20} />
              </span>
              <h3>{m.home.invitedTitle}</h3>
              <p>{m.home.invitedBody}</p>
              <form onSubmit={join}>
                <input
                  aria-label={m.home.codeLabel}
                  placeholder={m.home.codePlaceholder}
                  maxLength={8}
                  value={joinCode}
                  onChange={(e) => setJoinCode(e.target.value.toUpperCase())}
                  required
                />
                <Button className="dark" type="submit">
                  {m.home.joinSubmit}
                  <ArrowRight size={16} />
                </Button>
              </form>
            </section>
            <section className="yufolo-card">
              <div className="yufolo-card-top">
                <span>
                  <AudioLines size={19} />
                  YUFOLO × YUACTION
                </span>
                <Pill tone={config?.yufoloConnected ? "green" : "neutral"}>
                  {config?.yufoloConnected
                    ? m.home.yufoloLinked
                    : m.home.yufoloWaiting}
                </Pill>
              </div>
              <h3>{m.home.yufoloTitle}</h3>
              <p>
                {config?.yufoloConnected
                  ? m.home.yufoloLinkedBody
                  : m.home.yufoloWaitingBody}
              </p>
              <a href="https://yufolo.com" target="_blank" rel="noreferrer">
                {m.home.aboutYufolo}
                <ArrowRight size={15} />
              </a>
            </section>
          </div>
          <ErrorNote message={configError || (!creating ? error : "")} />
          {config?.demo && (
            <p className="preview-note">
              {m.home.demoNote}
            </p>
          )}
          <footer>
            <span>YuAction · BY COYUME</span>
            <span>{m.home.footer}</span>
          </footer>
        </div>
      </main>
      <dialog
        ref={dialog}
        aria-labelledby="create-activity-title"
        onCancel={() => setCreating(false)}
        onClose={() => setCreating(false)}
        className="create-dialog"
      >
        <form onSubmit={create}>
          <div className="dialog-heading">
            <div>
              <div className="eyebrow">NEW SESSION</div>
              <h2 id="create-activity-title">{m.home.create}</h2>
            </div>
            <button
              className="icon-button"
              type="button"
              aria-label={m.home.close}
              onClick={() => setCreating(false)}
            >
              <X size={20} />
            </button>
          </div>
          <label>
            {m.home.name}
            <input
              autoFocus
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder={m.home.namePlaceholder}
              maxLength={100}
              required
            />
          </label>
          <fieldset>
            <legend>{m.home.kind}</legend>
            <div className="kind-options">
              <button
                type="button"
                className={kind === "classroom" ? "selected" : ""}
                aria-pressed={kind === "classroom"}
                onClick={() => setKind("classroom")}
              >
                <GraduationCap size={24} />
                <strong>{m.home.classroomKind}</strong>
                <small>{m.home.classroomHint}</small>
              </button>
              <button
                type="button"
                className={kind === "talk" ? "selected" : ""}
                aria-pressed={kind === "talk"}
                onClick={() => setKind("talk")}
              >
                <Mic size={24} />
                <strong>{m.home.talkKind}</strong>
                <small>{m.home.talkHint}</small>
              </button>
            </div>
          </fieldset>
          {config?.creatorKeyRequired && (
            <label>
              {m.home.creatorKey}
              <input
                type="password"
                value={creatorKey}
                onChange={(e) => setCreatorKey(e.target.value)}
                required
                autoComplete="off"
              />
              <small>{m.home.creatorKeyHint}</small>
            </label>
          )}
          <ErrorNote message={error} />
          <Button className="primary full" type="submit" disabled={busy}>
            {busy ? m.home.creating : m.home.createEnter}
            <ArrowRight size={17} />
          </Button>
          <p className="form-note">{m.home.createNote}</p>
        </form>
      </dialog>
    </div>
  );
}
