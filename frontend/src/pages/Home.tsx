import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  ArrowRight,
  AudioLines,
  ChevronRight,
  GraduationCap,
  LayoutDashboard,
  MessageCircle,
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
        <div className="workspace-label">YOUR WORKSPACE</div>
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
          <span className="mini-label">BUILT FOR CONNECTION</span>
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
            <strong>YuAction 预览版</strong>
            <small>一起，让现场发生连接</small>
          </div>
        </div>
      </aside>
      <main className="home-main">
        <header className="topbar">
          <span>
            工作空间 <ChevronRight size={14} />
            <strong>活动概览</strong>
          </span>
          <Pill tone="neutral">EARLY PREVIEW · 0.1</Pill>
        </header>
        <div className="home-content">
          {config?.yufoloConnected && <AccountPanel onChange={setUser} />}
          <div className="page-heading">
            <div>
              <div className="eyebrow">A LITTLE MORE CONNECTION</div>
              <h1>
                让每一次表达，都有回应<span>。</span>
              </h1>
              <p>把提问、理解与交流，带回同一个现场。</p>
            </div>
            <Button
              className="primary"
              onClick={() => setCreating(true)}
              disabled={!config || (config.yufoloConnected && !user)}
            >
              <Plus size={18} />
              创建活动
            </Button>
          </div>
          <section className="hero-card">
            <div className="hero-copy">
              <Pill>
                <span className="tiny-dot" />
                为课堂与演讲而生
              </Pill>
              <h2>
                你专注讲述。
                <br />
                我们连接每一个人。
              </h2>
              <p>
                一个房间，让台上的表达与台下的思考相遇。
                <br />
                扫码参与、实时提问，让每个问题被看见。
              </p>
              <Button
                className="dark"
                onClick={() => setCreating(true)}
                disabled={!config || (config.yufoloConnected && !user)}
              >
                开启你的第一场互动
                <ArrowRight size={17} />
              </Button>
            </div>
            <div className="hero-art" aria-hidden="true">
              <div className="orbit orbit-one" />
              <div className="orbit orbit-two" />
              <div className="art-host">
                <span className="art-icon">
                  <Mic size={25} />
                </span>
                <div>
                  <strong>一个讲台</strong>
                  <small>开启连接</small>
                </div>
                <AudioLines size={26} />
              </div>
              <div className="art-note">
                <span className="avatar lavender">Y</span>
                <div>
                  让思考，加入对话。
                  <span className="art-bars">
                    <i />
                    <i />
                    <i />
                    <i />
                    <i />
                    <i />
                    <i />
                    <i />
                  </span>
                </div>
              </div>
              <div className="art-question">
                <MessageCircle size={17} />
                <span>这个部分，可以再讲讲吗？</span>
              </div>
              <div className="art-audience">
                <span className="avatar peach">A</span>
                <span className="avatar green">B</span>
                <span className="avatar lavender">C</span>
                <span className="audience-caption">每个人，都在现场</span>
              </div>
            </div>
          </section>
          <div className="section-title">
            <div>
              <h2>
                我的活动 <span>{rooms.length.toString().padStart(2, "0")}</span>
              </h2>
              <p>
                {config?.yufoloConnected
                  ? "此 Yufolo 账号创建的课堂与演讲"
                  : "此浏览器最近创建的课堂与演讲"}
              </p>
            </div>
            <span className="muted-label">一个房间，无限种交流</span>
          </div>
          <div className="activity-grid">
            {rooms.map((room) => (
              <a
                className="activity-card"
                href={`/rooms/${room.code}/host`}
                key={room.code}
              >
                <span className={`activity-icon ${room.kind}`}>
                  {room.kind === "classroom" ? (
                    <GraduationCap size={24} />
                  ) : (
                    <Mic size={24} />
                  )}
                </span>
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
              onClick={() => setCreating(true)}
              disabled={!config || (config.yufoloConnected && !user)}
            >
              <span>
                <Plus size={24} />
              </span>
              <strong>创建新的活动</strong>
              <small>下一次连接，从这里开始</small>
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
        onCancel={() => setCreating(false)}
        className="create-dialog"
      >
        <form onSubmit={create}>
          <div className="dialog-heading">
            <div>
              <div className="eyebrow">MAKE ROOM FOR IDEAS</div>
              <h2>开启一场新的连接</h2>
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
                onClick={() => setKind("classroom")}
              >
                <GraduationCap size={24} />
                <strong>课堂</strong>
                <small>让每个困惑被看见</small>
              </button>
              <button
                type="button"
                className={kind === "talk" ? "selected" : ""}
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
