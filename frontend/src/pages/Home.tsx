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

export default function Home() {
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
      setError("请输入 8 位房间码");
      return;
    }
    window.location.assign(`/rooms/${code}`);
  }
  return (
    <div className="app-shell">
      <aside className="sidebar">
        <Brand />
        <div className="workspace-label">工作空间</div>
        <nav>
          <a className="nav-item active" href="/">
            <LayoutDashboard size={18} />
            活动空间
          </a>
          <a className="nav-item" href="#join">
            <Users size={18} />
            加入活动
          </a>
          <a
            className="nav-item"
            href="https://yufolo.com"
            target="_blank"
            rel="noreferrer"
          >
            <AudioLines size={18} />
            Yufolo 学习空间
            <ArrowRight size={14} className="nav-arrow" />
          </a>
        </nav>
        <div className="sidebar-note">
          <span className="mini-label">专为真实的交流</span>
          <p>
            好的表达，
            <br />
            从听见彼此开始。
          </p>
          <span>BY COYUME</span>
        </div>
        <div className="sidebar-bottom">
          <span className="avatar">Y</span>
          <div>
            <strong>现场，从这里开始</strong>
            <small>课堂 · 演讲 · 每一次分享</small>
          </div>
        </div>
      </aside>
      <main className="home-main">
        <header className="topbar">
          <span className="workspace-breadcrumb">
            工作空间 <ChevronRight size={14} />
            <strong>活动概览</strong>
          </span>
          <div className="mobile-brand">
            <Brand />
          </div>
          <span className="topbar-note">
            <Radio size={14} /> 让交流发生在现场
          </span>
        </header>
        <div className="home-content">
          <div className="page-heading">
            <div>
              <div className="eyebrow">YOUR NEXT CONVERSATION</div>
              <h1>
                让每一次表达，都有回应<span>。</span>
              </h1>
              <p>创建一个活动，把实时字幕、现场提问和每一位听众连接起来。</p>
            </div>
            <div className="page-actions">
              <a href="#join" className="button outline">
                <Users size={16} />
                加入活动
              </a>
              <Button
                className="primary"
                onClick={beginCreate}
                disabled={!config}
              >
                <Plus size={18} />
                创建活动
              </Button>
            </div>
          </div>
          <div
            className={`welcome-grid ${config?.yufoloConnected ? "with-account" : ""} ${user ? "is-authenticated" : ""}`}
          >
            <section className="hero-card">
              <div className="hero-copy">
                <span className="hero-label">
                  <span className="tiny-dot" /> 一场讲述，多种回应
                </span>
                <h2>
                  你专注讲述。
                  <br />
                  <span>让全场，跟上你的想法。</span>
                </h2>
                <p>
                  字幕跟随声音，问题随时抵达。
                  <br />
                  为课堂和演讲，留出更多交流的空间。
                </p>
                <a href="#join" className="hero-link">
                  受邀参加？加入活动 <ArrowRight size={16} />
                </a>
              </div>
              <div className="hero-visual" aria-hidden="true">
                <div className="signal-ring ring-outer" />
                <div className="signal-ring ring-inner" />
                <div className="signal-center">
                  <AudioLines size={38} strokeWidth={1.5} />
                </div>
                <span className="signal-chip chip-caption">
                  <AudioLines size={15} /> 实时字幕
                </span>
                <span className="signal-chip chip-question">
                  <MessageCircle size={15} /> 现场提问
                </span>
                <span className="signal-chip chip-people">
                  <Users size={15} /> 共同参与
                </span>
              </div>
              <div className="hero-steps">
                <span>
                  <ScanLine size={16} /> 扫码加入
                </span>
                <i />
                <span>
                  <AudioLines size={16} /> 字幕同步
                </span>
                <i />
                <span>
                  <MessageCircle size={16} /> 随时提问
                </span>
              </div>
            </section>
            {config?.yufoloConnected && (
              <div className="account-slot" ref={accountSlot}>
                {awaitingLogin && (
                  <p className="account-prompt" role="status">
                    先登录，随后继续创建你的活动。
                  </p>
                )}
                <AccountPanel onChange={setUser} />
              </div>
            )}
          </div>
          <div className="section-title">
            <div>
              <h2>
                我的活动 <span>{rooms.length.toString().padStart(2, "0")}</span>
              </h2>
              <p>
                {config?.yufoloConnected
                  ? "你的课堂与演讲，都在这里"
                  : "此浏览器最近创建的课堂与演讲"}
              </p>
            </div>
            <span className="muted-label">随时回到你的现场</span>
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
                      {room.status === "live" ? "进行中" : "已结束"}
                    </Pill>
                  )}
                </div>
                <div className="activity-card-title">
                  <h3>{room.title}</h3>
                  <ArrowRight size={18} />
                </div>
                <p>
                  {room.kind === "classroom" ? "互动课堂" : "现场演讲"} ·{" "}
                  {new Date(room.createdAt).toLocaleDateString("zh-CN")}
                </p>
                <div className="activity-bottom">
                  <span>房间码</span>
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
              <strong>创建新的活动</strong>
              <small>
                {rooms.length
                  ? "为下一次分享准备一个空间"
                  : "给活动起个名字，邀请大家一起加入。"}
              </small>
            </button>
          </div>
          <div className="home-bottom-grid">
            <section className="join-card" id="join">
              <span className="small-icon">
                <Users size={20} />
              </span>
              <h3>受邀参加活动？</h3>
              <p>输入主持人分享的房间码，即可加入。</p>
              <form onSubmit={join}>
                <input
                  aria-label="房间码"
                  placeholder="输入 8 位房间码"
                  maxLength={8}
                  value={joinCode}
                  onChange={(e) => setJoinCode(e.target.value.toUpperCase())}
                  required
                />
                <Button className="dark" type="submit">
                  加入
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
                  {config?.yufoloConnected ? "已连接" : "等待配置"}
                </Pill>
              </div>
              <h3>一个人开讲，所有人跟上。</h3>
              <p>
                {config?.yufoloConnected
                  ? "用 Yufolo 账号登录，开启一次转录，让全场同步阅读原文与译文。"
                  : "连接 Yufolo 后，主持人可以开启麦克风，让全场共享同一份字幕。"}
              </p>
              <a href="https://yufolo.com" target="_blank" rel="noreferrer">
                了解 Yufolo
                <ArrowRight size={15} />
              </a>
            </section>
          </div>
          <ErrorNote message={configError || (!creating ? error : "")} />
          {config?.demo && (
            <p className="preview-note">
              已启用演示模式。演示字幕不调用识别服务；未配置数据库时，重启会清空活动。
            </p>
          )}
          <footer>
            <span>YuAction · BY COYUME</span>
            <span>每一次连接，都值得认真对待。</span>
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
              <h2 id="create-activity-title">创建活动</h2>
            </div>
            <button
              className="icon-button"
              type="button"
              aria-label="关闭"
              onClick={() => setCreating(false)}
            >
              <X size={20} />
            </button>
          </div>
          <label>
            活动名称
            <input
              autoFocus
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="例如：设计思维 · 第一堂课"
              maxLength={100}
              required
            />
          </label>
          <fieldset>
            <legend>活动类型</legend>
            <div className="kind-options">
              <button
                type="button"
                className={kind === "classroom" ? "selected" : ""}
                aria-pressed={kind === "classroom"}
                onClick={() => setKind("classroom")}
              >
                <GraduationCap size={24} />
                <strong>课堂</strong>
                <small>让每个困惑被看见</small>
              </button>
              <button
                type="button"
                className={kind === "talk" ? "selected" : ""}
                aria-pressed={kind === "talk"}
                onClick={() => setKind("talk")}
              >
                <Mic size={24} />
                <strong>演讲</strong>
                <small>与现场产生共鸣</small>
              </button>
            </div>
          </fieldset>
          {config?.creatorKeyRequired && (
            <label>
              活动创建密钥
              <input
                type="password"
                value={creatorKey}
                onChange={(e) => setCreatorKey(e.target.value)}
                required
                autoComplete="off"
              />
              <small>由部署管理员提供，仅用于创建活动。</small>
            </label>
          )}
          <ErrorNote message={error} />
          <Button className="primary full" type="submit" disabled={busy}>
            {busy ? "正在创建…" : "创建并进入工作台"}
            <ArrowRight size={17} />
          </Button>
          <p className="form-note">创建后即可分享房间码。参与者无需注册。</p>
        </form>
      </dialog>
    </div>
  );
}
